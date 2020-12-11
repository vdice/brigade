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
			Value: "/id_dsa",
			Usage: "Path to the ssh key to use for git auth (optional)",
		},
		&cli.StringFlag{
			Name:  "sshCert",
			Value: "/id_dsa-cert.pub",
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
		return fmt.Errorf("git config from %q is empty", payloadFile)
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
		keyFile, err := os.Stat(privateKeyFile)
		if err != nil && !os.IsNotExist(err) {
			return errors.Wrapf(err, "unable to read ssh key file %q", privateKeyFile)
		}

		if keyFile != nil {
			publicKeys, err := gitssh.NewPublicKeysFromFile(gitssh.DefaultUsername, privateKeyFile, "")
			if err != nil {
				return errors.Wrap(err, "error creating ssh auth for git")
			}
			publicKeys.HostKeyCallback = ssh.InsecureIgnoreHostKey()
			auth = publicKeys
		}
	}

	// Check for SSH Cert
	// https://github.blog/2019-08-14-ssh-certificate-authentication-for-github-enterprise-cloud/
	//
	// TODO: what mech will/can we receive the ssh cert
	// (BRIGADE_REPO_SSH_CERT in v1)
	// I think all we need to do is to make sure the cert file exists in the
	// below location (?)
	// sshCertFile := c.String("sshCert")

	// Prioritize using Ref; alternatively try Commit; else, set to master
	commitRef := gitConfig.Ref
	if commitRef == "" {
		commitRef = gitConfig.Commit
	}
	if commitRef == "" {
		commitRef = "refs/heads/master"
	}
	fullRef := plumbing.NewReferenceFromStrings(commitRef, commitRef)

	// Create initial refspec used for remote configs
	refSpec := config.RefSpec(fmt.Sprintf("+%s:%s", fullRef.Name(), fullRef.Name()))

	// Setup workspace using the osfs/filesystem impl, with the underlying
	// git storage as the usual workspace/.git dir
	//
	// fs = our repo's workspace/worktree
	// dotgit = the .git dir included therein
	//
	// TODO: v1 has env var for BRIGADE_WORKSPACE
	workspace := c.String("workspace")
	fs := osfs.New(workspace)
	dotgit := osfs.New(fs.Join(workspace, ".git"))
	gitStorage := filesystem.NewStorage(dotgit, cache.NewObjectLRUDefault())

	// If we're not dealing with an exact commit, list the remote refs
	// to build out a full, updated refspec
	if gitConfig.Commit == "" {
		// Create a new remote for the purposes of listing remote refs and finding
		// the full ref we want
		remoteConfig := &config.RemoteConfig{
			Name:  gitConfig.CloneURL,
			URLs:  []string{gitConfig.CloneURL},
			Fetch: []config.RefSpec{refSpec},
		}
		rem := git.NewRemote(gitStorage, remoteConfig)

		// List remote refs
		refs, err := rem.List(&git.ListOptions{Auth: auth})
		if err != nil {
			return errors.Wrap(err, "error listing remotes")
		}

		// Filter the list of refs and only keep the full ref matching our commitRef
		var found bool
		for _, ref := range refs {
			// Ignore the HEAD symbolic reference
			// e.g. [HEAD ref: refs/heads/master]
			if ref.Type() == plumbing.SymbolicReference {
				continue
			}

			if strings.Contains(ref.Name().String(), fullRef.Name().String()) ||
				strings.Contains(ref.Hash().String(), fullRef.Hash().String()) {
				fullRef = ref
				found = true
			}
		}

		if !found {
			return fmt.Errorf("reference %q not found in repo %q", fullRef.Name(), gitConfig.CloneURL)
		}

		// Create refspec with the updated ref
		refSpec = config.RefSpec(fmt.Sprintf("+%s:%s",
			fullRef.Name(), fullRef.Name()))
	}

	// Init empty git repo
	repo, err := git.Init(gitStorage, fs)
	if err != nil {
		return errors.Wrap(err, "error initing new git workspace")
	}

	// Create the remote that we'll use to fetch, using the updated/full refspec
	remoteConfig := &config.RemoteConfig{
		Name:  gitConfig.CloneURL,
		URLs:  []string{gitConfig.CloneURL},
		Fetch: []config.RefSpec{refSpec},
	}
	remote, err := repo.CreateRemote(remoteConfig)
	if err != nil {
		return errors.Wrap(err, "error creating remote")
	}

	// Fetch the ref specs we are interested in
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

	// Create a FETCH_HEAD reference pointing to our ref hash
	// The go-git library doesn't appear to support adding this, though the
	// git CLI does.  This is for parity with v1 functionality.
	//
	// From https://git-scm.com/docs/gitrevisions:
	// "FETCH_HEAD records the branch which you fetched from a remote repository
	// with your last git fetch invocation.""
	newRef := plumbing.NewReferenceFromStrings("FETCH_HEAD",
		fullRef.Hash().String())
	err = repo.Storer.SetReference(newRef)
	if err != nil {
		return errors.Wrap(err, "unable to set ref")
	}

	// Create worktree/repo contents
	worktree, err := repo.Worktree()
	if err != nil {
		return errors.Wrap(err, "error creating repo worktree")
	}

	// Check out worktree/repo contents
	checkoutOpts := &git.CheckoutOptions{
		Branch: fullRef.Name(),
		Force:  true,
	}
	err = worktree.Checkout(checkoutOpts)
	if err != nil {
		return errors.Wrap(err, "error checking out worktree")
	}

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
				return errors.Wrapf(err, "error updating submodule %q: %s",
					submodule.Config().Name)
			}
		}
	}

	return nil
}
