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
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	"golang.org/x/crypto/ssh"

	"github.com/brigadecore/brigade/sdk/v2/core"
	"github.com/brigadecore/brigade/v2/vcs-sidecar/version"
)

type event struct {
	Worker worker `json:"worker"`
}

type worker struct {
	Git *core.GitConfig `json:"git"`
}

// TODO: needs retry (see v1)

// TODO: need env vars that v1 uses?
// // BRIGADE_WORKSPACE
// // askpass.sh
// // gitssh.sh

func main() {
	log.Printf(
		"Starting Brigade VCS Sidecar -- version %s -- commit %s",
		version.Version(),
		version.Commit(),
	)

	payloadFile := "/event.json"
	data, err := ioutil.ReadFile(payloadFile)
	if err != nil {
		log.Fatal(err)
	}

	var event event
	err = json.Unmarshal(data, &event)
	if err != nil {
		log.Fatalf("error unmarshaling the event: %s", err)
	}

	// Extract git config
	gitConfig := event.Worker.Git
	if gitConfig == nil {
		log.Fatal("git config from event.json is nil")
	}

	// Setup Auth
	var auth transport.AuthMethod
	// TODO: auth token-based?  (see v1 askpass.sh)
	// Do we default to this or just nil auth?
	auth = &githttp.BasicAuth{
		// TODO: what mech will/can we receive this token (BRIGADE_REPO_AUTH_TOKEN in v1)
		// TODO: this appears to be used as an oauth token only -- or can it also be used as an ssh passphrase when paired with a key?
		Password: os.Getenv("BRIGADE_REPO_AUTH_TOKEN"),
	}

	// Check for SSH Key
	// TODO: what mech will/can we receive the ssh key (BRIGADE_REPO_KEY in v1)
	privateKeyFile := "/id_dsa"
	if _, err = os.Stat(privateKeyFile); err == nil {
		publicKeys, err := gitssh.NewPublicKeysFromFile(gitssh.DefaultUsername, privateKeyFile, "")
		if err != nil {
			log.Fatalf("error creating public ssh keys: %s", err)
		}
		publicKeys.HostKeyCallback = ssh.InsecureIgnoreHostKey()
		auth = publicKeys
	}

	// Check for SSH Cert
	// see https://github.blog/2019-08-14-ssh-certificate-authentication-for-github-enterprise-cloud/ for more details
	// TODO: what mech will/can we receive the ssh cert (BRIGADE_REPO_SSH_CERT in v1)
	// I think all we need to do is to make sure the cert file exists in the below location (?)
	// sshCertFile := "/id_dsa-cert.pub"

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
		log.Fatalf("error validating refSpec %q from commitRef %q: %s", refSpec, commitRef, err)
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
		log.Fatalf("error listing remotes: %s", err)
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
		log.Fatalf("error validating refSpec %q from commitRef %q: %s", refSpec, commitRef, err)
	}

	// TODO: v1 has env var for BRIGADE_WORKSPACE
	workspace := "/src"
	repo, err := git.Init(gitStorage, osfs.New(workspace))
	if err != nil {
		log.Fatalf("error initing new git workspace: %s", err)
	}

	remote, err := repo.CreateRemote(remoteConfig)
	if err != nil {
		log.Fatalf("error creating remote: %s", err)
	}

	fetchOpts := &git.FetchOptions{
		RemoteName: gitConfig.CloneURL,
		RefSpecs:   []config.RefSpec{refSpec},
		Force:      true,
		Auth:       auth,
	}
	err = remote.Fetch(fetchOpts)
	if err != nil {
		log.Fatalf("error fetching remotes: %s", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		log.Fatalf("error creating repo worktree: %s", err)
	}

	checkoutOpts := &git.CheckoutOptions{
		Branch: plumbing.ReferenceName(commitRef),
		Force:  true,
	}
	err = worktree.Checkout(checkoutOpts)
	if err != nil {
		log.Fatalf("error checking out worktree: %s", err)
	}

	// Init submodules if config'd to do so
	if gitConfig.InitSubmodules {
		submodules, err := worktree.Submodules()
		if err != nil {
			log.Fatalf("error retrieving submodules: %s", err)
		}
		opts := &git.SubmoduleUpdateOptions{
			Init:              true,
			RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
		}
		for _, submodule := range submodules {
			err := submodule.Update(opts)
			if err != nil {
				log.Fatalf("error updating submodule %q: %s", submodule.Config().Name, err)
			}
		}
	}

	// Testing
	cmd := exec.Command("ls", "-haltr", "/src")
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	if err != nil {
		log.Fatalf("failed to run command %q: %s", cmd, err)
	}
	log.Print(out.String())
}
