package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strings"

	osfs "github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
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
	fmt.Println()
	if err := app.RunContext(signals.Context(), os.Args); err != nil {
		fmt.Printf("\n%s\n\n", err)
		os.Exit(1)
	}
	fmt.Println()
}

// TODO: needs retry (see v1)

// TODO: need env vars that v1 uses?
// // BRIGADE_WORKSPACE

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

	// Direct port of v1 logic follows below:
	commitRef := gitConfig.Ref
	if commitRef == "" {
		commitRef = gitConfig.Commit
	}

	// Create refspec
	refSpec := config.RefSpec(fmt.Sprintf("+%s:%s",
		strings.TrimSpace(commitRef), strings.TrimSpace(commitRef)))

	err = refSpec.Validate()
	if err != nil {
		return errors.Wrapf(err, "error validating refSpec %q from commitRef %q", refSpec, commitRef)
	}

	gitStorage := memory.NewStorage()
	remoteConfig := &config.RemoteConfig{
		Name:  "origin",
		URLs:  []string{gitConfig.CloneURL},
		Fetch: []config.RefSpec{refSpec},
	}

	// Create the remote with repository URL
	rem := git.NewRemote(gitStorage, remoteConfig)

	// From v1: git ls-remote --exit-code "${BRIGADE_REMOTE_URL}" "${BRIGADE_COMMIT_REF}" | cut -f2
	refs, err := rem.List(&git.ListOptions{Auth: auth})
	if err != nil {
		return errors.Wrap(err, "error listing remotes")
	}

	// Filter the references list and only keep the full ref
	// matching our commitRef
	for _, ref := range refs {
		// There appears to be a HEAD ref that may exist with formatting
		// different from the rest; if we encounter, skip
		// [HEAD ref: refs/heads/master]
		if strings.Contains(ref.Strings()[0], "HEAD") {
			continue
		}

		if strings.Contains(ref.Strings()[0], commitRef) ||
			strings.Contains(ref.Strings()[1], commitRef) {
			commitRef = ref.Strings()[1]
		}
	}

	// Create refspec
	refSpec = config.RefSpec(fmt.Sprintf("+%s:%s",
		strings.TrimSpace(commitRef), strings.TrimSpace(commitRef)))

	err = refSpec.Validate()
	if err != nil {
		return errors.Wrapf(err, "error validating refSpec %q from commitRef %q", refSpec, commitRef)
	}

	// TODO: v1 has env var for BRIGADE_WORKSPACE
	workspace := c.String("workspace")
	repo, err := git.Init(gitStorage, osfs.New(workspace))
	if err != nil {
		return errors.Wrap(err, "error initing new git workspace")
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
		return errors.Wrap(err, "error fetching remotes")
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return errors.Wrap(err, "error creating repo worktree")
	}

	checkoutOpts := &git.CheckoutOptions{
		Branch: plumbing.ReferenceName(commitRef),
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
				return errors.Wrapf(err, "error updating submodule %q: %s", submodule.Config().Name)
			}
		}
	}

	// Testing
	cmd := exec.Command("ls", "-haltr", "/src")
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	if err != nil {
		return errors.Wrapf(err, "failed to run command %q", cmd)
	}
	log.Print(out.String())

	return nil
}
