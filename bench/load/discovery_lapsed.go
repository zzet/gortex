package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// This declaration is only an input to owned root preparation. Neither it nor
// the resulting no-store descriptor constitutes an observed catalog identity.
type discoveryTopology struct {
	Mode  string                  `json:"mode"`
	Roots []discoveryTopologyRoot `json:"roots"`
}

type discoveryTopologyRoot struct {
	ID          string `json:"id"`
	Role        string `json:"role"`
	Kind        string `json:"corpus_kind"`
	CorpusLabel string `json:"corpus_label,omitempty"`
	Family      string `json:"family"`
	Nested      bool   `json:"nested,omitempty"`
}

// Support clones are explicitly unconfigured and have no worktree content.
type discoverySupportRoot struct {
	Path       string `json:"path"`
	CommonDir  string `json:"git_common_dir"`
	HEAD       string `json:"head"`
	Configured bool   `json:"configured"`
	Live       bool   `json:"live"`
}

func (d discoveryTopology) validate(corpora []Corpus) error {
	if d.Mode != "literal_lapsed_v1" || len(d.Roots) != 29 || len(corpora) != 7 {
		return errors.New("literal lapsed preparation requires explicit 27 live plus two stale roots and seven pinned corpora")
	}
	validID := regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	byLabel := map[string]Corpus{}
	for _, corpus := range corpora {
		if err := validateCorpus(corpus); err != nil {
			return err
		}
		if !validID.MatchString(corpus.Label) || byLabel[corpus.Label].Label != "" {
			return errors.New("literal lapsed corpus labels must be unique path components")
		}
		byLabel[corpus.Label] = corpus
	}
	ids, families, used := map[string]bool{}, map[string]discoveryTopologyRoot{}, map[string]bool{}
	counts, kinds := map[string]int{}, map[string]int{}
	for _, row := range d.Roots {
		if !validID.MatchString(row.ID) || ids[row.ID] {
			return errors.New("duplicate or invalid declared root ID")
		}
		ids[row.ID] = true
		counts[row.Role]++
		switch row.Role {
		case "primary", "dedicated":
			if row.Family != row.ID || row.Nested || byLabel[row.CorpusLabel].Label == "" || used[row.CorpusLabel] {
				return errors.New("each declared graph owner requires its own pinned corpus and family")
			}
			used[row.CorpusLabel] = true
			families[row.ID] = row
			kinds[row.Role+":"+row.Kind]++
		case "linked", "stale":
			if row.CorpusLabel != "" || row.Kind != "gortex" || row.Role == "stale" && row.Nested {
				return errors.New("linked/stale roots must declare the live gortex family")
			}
		default:
			return errors.New("unknown declared topology role")
		}
	}
	if counts["primary"] != 3 || counts["dedicated"] != 4 || counts["linked"] != 20 || counts["stale"] != 2 || kinds["primary:gortex"] != 1 || kinds["primary:pdf"] != 1 || kinds["primary:sql"] != 1 || kinds["dedicated:go"] != 2 || kinds["dedicated:pdf"] != 1 || kinds["dedicated:sql"] != 1 {
		return errors.New("literal lapsed role/corpus vector differs from 3 primary, 20 linked, 4 dedicated, 2 stale")
	}
	for _, row := range d.Roots {
		if row.Role == "dedicated" && ids["support-"+row.ID] {
			return errors.New("declared root aliases a reserved dedicated support clone")
		}
		if row.Role == "linked" || row.Role == "stale" {
			family, ok := families[row.Family]
			if !ok || family.Role != "primary" || family.Kind != "gortex" {
				return errors.New("linked/stale root names an undeclared gortex primary family")
			}
		}
	}
	return nil
}

func prepareLapsedDiscoveryState(ctx context.Context, o runOptions, corpora []Corpus) error {
	if err := o.Discovery.validate(checkoutDiscoveryScenario, 0); err != nil {
		return err
	}
	var topology discoveryTopology
	if err := discoveryReadJSON(o.Discovery.Topology, &topology); err != nil {
		return err
	}
	if err := topology.validate(corpora); err != nil {
		return err
	}
	if err := validateOutputRoots(corpora, o.Scratch, o.Discovery.Prepare); err != nil {
		return err
	}
	if _, err := os.Lstat(o.Discovery.Prepare); !errors.Is(err, os.ErrNotExist) {
		return errors.New("discovery descriptor destination must be absent")
	}
	byLabel := map[string]Corpus{}
	for _, corpus := range corpora {
		byLabel[corpus.Label] = corpus
	}
	var primaries []Corpus
	for _, row := range topology.Roots {
		if row.Role == "primary" {
			corpus := byLabel[row.CorpusLabel]
			corpus.Label = row.ID
			primaries = append(primaries, corpus)
		}
	}
	p, err := newPrivateRun(ctx, o.Scratch, o.Binary, primaries)
	if err != nil {
		return err
	}
	defer os.RemoveAll(p.SocketDir)
	complete := false
	defer func() {
		if !complete {
			_ = p.removeState()
		}
	}()
	p.editACKMode = o.editACKPolicy()
	prepared := map[string]privateRepo{}
	for _, repo := range p.Repos {
		repo.Prefix = repo.Corpus.Label
		prepared[repo.Corpus.Label] = repo
	}
	var supports []discoverySupportRoot
	// All owner families are materialized before declared linked/stale entries;
	// declaration order never confers graph ownership or family membership.
	for _, row := range topology.Roots {
		if row.Role != "dedicated" {
			continue
		}
		corpus := byLabel[row.CorpusLabel]
		support := filepath.Join(p.Root, "repos", "support-"+row.ID)
		if _, err := p.git(ctx, "", "clone", "--no-hardlinks", "--no-checkout", "--", corpus.Source, support); err != nil {
			return err
		}
		if _, err := p.git(ctx, support, "update-ref", "--no-deref", "HEAD", corpus.Commit); err != nil {
			return err
		}
		path := filepath.Join(p.Root, "repos", row.ID)
		if _, err := p.git(ctx, support, "worktree", "add", "--detach", path, corpus.Commit); err != nil {
			return err
		}
		file, err := ownedFile(path, corpus.File)
		if err != nil {
			return err
		}
		common, err := p.git(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return err
		}
		supports = append(supports, discoverySupportRoot{Path: support, CommonDir: strings.TrimSpace(string(common)), HEAD: corpus.Commit})
		corpus.Label = row.ID
		prepared[row.ID] = privateRepo{Corpus: corpus, Path: path, File: file, Prefix: row.ID}
	}
	for _, row := range topology.Roots {
		if row.Role != "linked" && row.Role != "stale" {
			continue
		}
		primary := prepared[row.Family]
		path := filepath.Join(p.Root, "repos", row.ID)
		if row.Nested {
			path = filepath.Join(primary.Path, ".load-existing", row.ID)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
		}
		if _, err := p.git(ctx, primary.Path, "worktree", "add", "--detach", path, primary.Corpus.Commit); err != nil {
			return err
		}
		file, err := ownedFile(path, primary.Corpus.File)
		if err != nil {
			return err
		}
		corpus := primary.Corpus
		corpus.Label = row.ID
		prepared[row.ID] = privateRepo{Corpus: corpus, Path: path, File: file, Prefix: primary.Prefix}
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	d := discoverySeedDescriptor{State: "PREPARED_ROOTS_NO_STORE", Root: p.Root, Nonce: hex.EncodeToString(token), Topology: topology.Mode, LiveCount: 27, PrimaryCount: 3, LinkedCount: 20, DedicatedCount: 4, StaleCount: 2, TotalCount: 29, SupportRoots: supports}
	d.TopologySHA256, err = discoveryFileSHA(o.Discovery.Topology)
	if err != nil {
		return err
	}
	var configured []privateRepo
	p.Repos = nil
	for _, row := range topology.Roots {
		repo := prepared[row.ID]
		common, err := p.git(ctx, repo.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return err
		}
		head, err := p.git(ctx, repo.Path, "rev-parse", "HEAD")
		if err != nil || strings.TrimSpace(string(head)) != repo.Corpus.Commit {
			return errors.New("prepared lapsed HEAD differs from source pin")
		}
		sha, err := discoveryFileSHA(repo.File)
		if err != nil {
			return err
		}
		isConfigured := row.Role == "primary" || row.Role == "dedicated"
		viewKind, checkoutKind := "worktree", "linked_worktree"
		if isConfigured {
			viewKind = "base"
		}
		if row.Role == "primary" {
			checkoutKind = "primary"
		}
		d.Repos = append(d.Repos, discoverySeedRepo{Repo: repo, Role: row.Role, FileSHA256: sha, CommonDir: strings.TrimSpace(string(common)), ID: row.ID, Kind: row.Kind, Family: row.Family, ViewKind: viewKind, CheckoutKind: checkoutKind, Configured: isConfigured, Live: row.Role != "stale"})
		p.Repos = append(p.Repos, repo)
		if isConfigured {
			configured = append(configured, repo)
		}
		if row.Nested {
			d.NestedCount++
		}
	}
	boot := *p
	boot.Repos = configured
	if _, err := boot.writeBootRepos(false); err != nil {
		return err
	}
	for path, target := range map[string]*string{p.Config: &d.ConfigSHA256, filepath.Join(p.Root, "config", "gortex", "config.yaml"): &d.BootConfigSHA256, filepath.Join(p.Root, "gitconfig"): &d.GitConfigSHA256} {
		*target, err = discoveryFileSHA(path)
		if err != nil {
			return err
		}
	}
	if err := discoveryWriteJSON(filepath.Join(p.Root, "discovery-owner.json"), d); err != nil {
		return err
	}
	if err := discoveryWriteJSON(o.Discovery.Prepare, d); err != nil {
		return err
	}
	complete = true
	fmt.Printf("prepared owned lapsed discovery descriptor %s; no daemon or store was opened\n", o.Discovery.Prepare)
	return nil
}
