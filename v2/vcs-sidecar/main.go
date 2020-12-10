package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	osfs "github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"

	cli "github.com/urfave/cli/v2"

	"github.com/brigadecore/brigade/sdk/v2/core"
	"github.com/brigadecore/brigade/v2/internal/signals"
	"github.com/brigadecore/brigade/v2/vcs-sidecar/version"
)

type event struct {
	Worker worker `json:"worker"`
}

type worker struct {
	Git *core.GitConfig `json:"git"`
}

// Building w/ CLI frontend such that various opts can be easily overridden
// for testing, e.g. payload file location, workspace location, etc.
func main() {
	app := cli.NewApp()
	app.Name = "Brigade VCS Sidecar"
	app.Usage = "VCS checkout utility for Brigade"
	app.Version = fmt.Sprintf(
		"%s -- commit %s",
		version.Version(),
		version.Commit(),
	)
	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:    "payload",
			Aliases: []string{"p"},
			Value:   "/event.json",
			Usage:   "Path to the payload to extract vcs details (default: `/event.json`)",
		},
		&cli.StringFlag{
			Name:    "workspace",
			Aliases: []string{"w"},
			Value:   "/src",
			Usage:   "Path representing where to set up the VCS workspace (default: `/src`)",
		},
		&cli.StringFlag{
			Name:  "sshKey",
			Usage: "Path to the ssh key to use for git auth (optional)",
		},
		&cli.StringFlag{
			Name:  "sshCert",
			Usage: "Path to the ssh cert to use for git auth (optional)",
		},
	}
	app.Action = vcsCheckout
	if err := app.RunContext(signals.Context(), os.Args); err != nil {
		fmt.Printf("\n%s\n\n", err)
		os.Exit(1)
	}
	fmt.Println()
}

// TODO: needs retry (see v1)

func vcsCheckout(c *cli.Context) error {
	payloadFile := c.String("payload")
	data, err := ioutil.ReadFile(payloadFile)
	if err != nil {
		return errors.Wrapf(err, "unable read the payload file %q", payloadFile)
	}

	var event event
	err = json.Unmarshal(data, &event)
	if err != nil {
		return errors.Wrap(err, "error unmarshaling the event")
	}

	// Extract git config
	gitConfig := event.Worker.Git
	if gitConfig == nil {
		return fmt.Errorf("git config from %q is nil", payloadFile)
	}

	// Setup Auth
	var auth transport.AuthMethod
	// TODO: auth token-based?  (see v1 askpass.sh)
	// Do we default to this or just nil auth?
	// auth = &githttp.BasicAuth{
	// 	// TODO: what mech will/can we receive this token (BRIGADE_REPO_AUTH_TOKEN in v1)
	// 	// TODO: this appears to be used as an oauth token only -- or can it also be used as an ssh passphrase when paired with a key?
	// 	Password: os.Getenv("BRIGADE_REPO_AUTH_TOKEN"),
	// }

	// Check for SSH Key
	// TODO: what mech will/can we receive the ssh key (BRIGADE_REPO_KEY in v1)
	privateKeyFile := c.String("sshKey")
	if privateKeyFile != "" {
		_, err = os.Stat(privateKeyFile)
		if err != nil {
			return errors.Wrapf(err, "unable to locate ssh key file %q", privateKeyFile)
		}

		publicKeys, err := gitssh.NewPublicKeysFromFile(gitssh.DefaultUsername, privateKeyFile, "")
		if err != nil {
			return errors.Wrap(err, "error creating ssh auth for git")
		}
		publicKeys.HostKeyCallback = ssh.InsecureIgnoreHostKey()
		auth = publicKeys
	}

	// Check for SSH Cert
	// see https://github.blog/2019-08-14-ssh-certificate-authentication-for-github-enterprise-cloud/ for more details
	// TODO: what mech will/can we receive the ssh cert (BRIGADE_REPO_SSH_CERT in v1)
	// I think all we need to do is to make sure the cert file exists in the below location (?)
	// sshCertFile := c.String("sshCert")

	// Prioritize using Ref; alternatively try Commit
	commitRef := gitConfig.Ref
	if commitRef == "" {
		commitRef = gitConfig.Commit
	}

	// Create refspec
	refSpec := config.RefSpec(fmt.Sprintf("+%s:%s", commitRef, commitRef))

	err = refSpec.Validate()
	if err != nil {
		return errors.Wrapf(err, "error validating refSpec %q from commitRef %q", refSpec, commitRef)
	}

	// TODO: v1 has env var for BRIGADE_WORKSPACE
	workspace := c.String("workspace")
	fs := osfs.New(workspace)
	gitStorage := filesystem.NewStorage(osfs.New(fs.Join(workspace, ".git")), cache.NewObjectLRUDefault())

	// TODO: rm debug log
	fmt.Printf("workspace = %s\n", workspace)

	// Create a new remote for the purposes of listing remote refs and finding
	// the full ref we want
	remoteConfig := &config.RemoteConfig{
		Name:  gitConfig.CloneURL,
		URLs:  []string{gitConfig.CloneURL},
		Fetch: []config.RefSpec{refSpec},
	}
	rem := git.NewRemote(gitStorage, remoteConfig)

	refs, err := rem.List(&git.ListOptions{Auth: auth})
	if err != nil {
		return errors.Wrap(err, "error listing remotes")
	}

	// Filter the references list and only keep the full ref
	// matching our commitRef
	var fullRef string
	for _, ref := range refs {
		// Ignore the HEAD symbolic reference
		// e.g. [HEAD ref: refs/heads/master]
		if ref.Type() == plumbing.SymbolicReference {
			continue
		}

		if strings.Contains(ref.Strings()[0], commitRef) ||
			strings.Contains(ref.Strings()[1], commitRef) {
			fullRef = ref.Strings()[0]
		}
	}
	// Create refspec
	refSpec = config.RefSpec(fmt.Sprintf("+%s:%s", fullRef, fullRef))
	// TODO: rm debug log
	fmt.Printf("refSpec = %+v\n", refSpec)
	err = refSpec.Validate()
	if err != nil {
		return errors.Wrapf(err, "error validating refSpec %q", refSpec)
	}

	repo, err := git.Init(gitStorage, fs)
	if err != nil {
		return errors.Wrap(err, "error initing new git workspace")
	}

	// Create the remote that we'll use to fetch, using the updated/full refspec
	remoteConfig = &config.RemoteConfig{
		Name:  gitConfig.CloneURL,
		URLs:  []string{gitConfig.CloneURL},
		Fetch: []config.RefSpec{refSpec},
	}
	remote, err := repo.CreateRemote(remoteConfig)
	if err != nil {
		return errors.Wrap(err, "error creating remote")
	}

	fetchOpts := &git.FetchOptions{
		RemoteName: gitConfig.CloneURL,
		RefSpecs:   []config.RefSpec{refSpec},
		Force:      true,
		Auth:       auth,
	}
	err = remote.Fetch(fetchOpts)
	if err != nil {
		return errors.Wrap(err, "error fetching refs from the remote")
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return errors.Wrap(err, "error creating repo worktree")
	}

	// Apparently w/ go-git, if we have a tag, we should check out via hash?
	// https://github.com/src-d/go-git/issues/980#issuecomment-434249875
	//
	// If we just check out the branch (e.g. v0.1.0), the tag test in test/test.sh
	// errors with:
	// // Check failed: 'ddff78a' == '589e150' (expected is 'ddff78a')
	//
	// However, when we attempt to checkout via the hash (which is 'ddff78a')
	// We see the following from go-git:
	// // error checking out worktree: object not found

	// See if we have a tag
	tag, err := repo.Tag(commitRef)
	if err != nil && err != git.ErrTagNotFound {
		return errors.Wrap(err, "unable to get tag object from commit reference")
	}

	checkoutOpts := &git.CheckoutOptions{
		Force: true,
	}
	if tag != nil {
		checkoutOpts.Hash = tag.Hash()
	} else {
		checkoutOpts.Branch = plumbing.ReferenceName(fullRef)
	}

	// TODO: rm debug log
	fmt.Printf("checkoutOpts = %+v\n", checkoutOpts)

	err = worktree.Checkout(checkoutOpts)
	if err != nil {
		return errors.Wrap(err, "error checking out worktree")
	}

	// TODO: after we checkout, we only have a HEAD sym ref,
	// no FETCH_HEAD and no ORIG_HEAD
	// (areese's v1 tests used FETCH_HEAD)

	// Init submodules if config'd to do so
	if gitConfig.InitSubmodules {
		submodules, err := worktree.Submodules()
		if err != nil {
			return errors.Wrap(err, "error retrieving submodules")
		}
		opts := &git.SubmoduleUpdateOptions{
			Init:              true,
			RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
		}
		for _, submodule := range submodules {
			err := submodule.Update(opts)
			if err != nil {
				return errors.Wrapf(err, "error updating submodule %q: %s", submodule.Config().Name)
			}
		}
	}

	return nil
}
