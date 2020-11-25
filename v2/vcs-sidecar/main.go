package main

import (
	"fmt"
	"log"
	"strings"

	osfs "github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"

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

func main() {
	log.Printf(
		"Starting Brigade VCS Sidecar -- version %s -- commit %s",
		version.Version(),
		version.Commit(),
	)

	// TODO: Read and unmarshal event payload
	// payloadFile := "event.json"
	// bytes, err := ioutil.ReadFile(payloadFile)
	// if err != nil {
	// 	log.Fatal(err)
	// }

	// var event event
	// err = json.Unmarshal(bytes, &event)
	// if err != nil {
	// 	log.Fatal(err)
	// }

	// Extract git config
	// gitConfig := event.Worker.Git
	// temporary for testing:
	gitConfig := core.GitConfig{
		Ref:            "master",
		CloneURL:       "https://github.com/sgoings/makeup.git",
		InitSubmodules: true,
	}

	// Direct port of v1 logic:

	commitRef := gitConfig.Ref
	if commitRef == "" {
		commitRef = gitConfig.Commit
	}

	fmt.Printf("commitRef = %s\n", commitRef)

	// Create refspec
	refSpec := config.RefSpec(fmt.Sprintf("+%s:%s",
		strings.TrimSpace(commitRef), strings.TrimSpace(commitRef)))

	err := refSpec.Validate()
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

	refs, err := rem.List(&git.ListOptions{})
	if err != nil {
		log.Fatalf("error listing remotes: %s", err)
	}

	// Filters the references list and only keep the full ref
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
			// From v1: git ls-remote --exit-code "${BRIGADE_REMOTE_URL}" "${BRIGADE_COMMIT_REF}" | cut -f2
			commitRef = ref.Strings()[1]
		}
	}

	refSpec = config.RefSpec(fmt.Sprintf("+%s:%s",
		strings.TrimSpace(commitRef), strings.TrimSpace(commitRef)))

	err = refSpec.Validate()
	if err != nil {
		log.Fatalf("error validating refSpec %q from commitRef %q: %s", refSpec, commitRef, err)
	}

	// TODO: v1 had env var for BRIGADE_WORKSPACE
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

	if gitConfig.InitSubmodules {
		// git submodule update --init --recursive
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
}
