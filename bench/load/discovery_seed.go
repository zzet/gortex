package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
)

type discoverySeedRepo struct {
	Repo       privateRepo `json:"repo"`
	Role       string      `json:"role"`
	FileSHA256 string      `json:"file_sha256"`
	CommonDir  string      `json:"git_common_dir"`
}
type discoverySeedDescriptor struct {
	State            string              `json:"state"`
	Root             string              `json:"owned_root"`
	Nonce            string              `json:"owner_nonce"`
	ConfigSHA256     string              `json:"config_sha256"`
	BootConfigSHA256 string              `json:"boot_config_sha256"`
	GitConfigSHA256  string              `json:"git_config_sha256"`
	Repos            []discoverySeedRepo `json:"repositories"`
	PrimaryCount     int                 `json:"primary_checkout_count"`
	LinkedCount      int                 `json:"linked_checkout_count"`
	NestedCount      int                 `json:"nested_linked_checkout_count"`
	TotalCount       int                 `json:"total_checkout_count"`
}
type discoverySeedLineage struct {
	State                 string              `json:"state"`
	Descriptor            string              `json:"descriptor"`
	DescriptorSHA256      string              `json:"descriptor_sha256"`
	OwnedRoot             string              `json:"owned_root"`
	SourceGeneration      string              `json:"source_generation"`
	OldVersion            string              `json:"old_version"`
	OldBinarySHA256       string              `json:"old_binary_sha256"`
	UpgradeSourceCommit   string              `json:"upgrade_source_commit"`
	UpgradeBinarySHA256   string              `json:"upgrade_binary_sha256"`
	Snapshot              string              `json:"snapshot"`
	MainSHA256            string              `json:"main_sha256"`
	WALSHA256             string              `json:"wal_sha256"`
	SchemaVersion         int                 `json:"schema_version"`
	NormalStop            bool                `json:"normal_stop"`
	OwnersExited          bool                `json:"owners_exited"`
	OwnerPID              int                 `json:"owner_pid"`
	StopReceipt           string              `json:"stop_receipt"`
	StopReceiptSHA256     string              `json:"stop_receipt_sha256"`
	IdentityReceipt       string              `json:"identity_receipt"`
	IdentityReceiptSHA256 string              `json:"identity_receipt_sha256"`
	Repos                 []discoverySeedRepo `json:"repositories"`
}

func discoveryFileSHA(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("discovery lineage requires regular files, not symlinks")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	if err := errors.Join(copyErr, f.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func discoveryReadJSON(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 4<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("discovery descriptor must contain exactly one JSON object")
	}
	return nil
}
func discoveryWriteJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return errors.Join(err, f.Close())
}

// Preparation only makes owned clones/topology and metadata. The exclusive
// producer must populate and upgrade this exact root before resume can pass.
func prepareDiscoveryState(ctx context.Context, o runOptions, corpora []Corpus) error {
	if len(corpora) < 2 {
		return errors.New("discovery preparation requires two unrelated pinned corpora")
	}
	if err := validateOutputRoots(corpora, o.Discovery.Prepare); err != nil {
		return err
	}
	if _, err := os.Lstat(o.Discovery.Prepare); !errors.Is(err, os.ErrNotExist) {
		return errors.New("discovery descriptor destination must be absent")
	}
	p, err := newPrivateRun(ctx, o.Scratch, o.Binary, corpora[:2])
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
	for i := range o.Discovery.ExistingWorktrees {
		if err := p.addExistingDiscoveryWorktree(ctx, i, i < o.Discovery.ExistingNested); err != nil {
			return err
		}
	}
	if _, err := p.writeDiscoveryBootRepos(); err != nil {
		return err
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	d := discoverySeedDescriptor{State: "PREPARED_ROOTS_NO_STORE", Root: p.Root, Nonce: hex.EncodeToString(token), PrimaryCount: 2, LinkedCount: len(p.Repos) - 2, NestedCount: o.Discovery.ExistingNested, TotalCount: len(p.Repos)}
	d.ConfigSHA256, err = discoveryFileSHA(p.Config)
	if err != nil {
		return err
	}
	d.BootConfigSHA256, err = discoveryFileSHA(filepath.Join(p.Root, "config", "gortex", "config.yaml"))
	if err != nil {
		return err
	}
	d.GitConfigSHA256, err = discoveryFileSHA(filepath.Join(p.Root, "gitconfig"))
	if err != nil {
		return err
	}
	for i, repo := range p.Repos {
		role := "existing_primary_family"
		repo.Prefix = p.Repos[0].Corpus.Label
		if i < 2 {
			role = "primary"
			repo.Prefix = repo.Corpus.Label
		}
		sha, err := discoveryFileSHA(repo.File)
		if err != nil {
			return err
		}
		common, err := p.git(ctx, repo.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return err
		}
		d.Repos = append(d.Repos, discoverySeedRepo{Repo: repo, Role: role, FileSHA256: sha, CommonDir: strings.TrimSpace(string(common))})
	}
	if err := discoveryWriteJSON(filepath.Join(p.Root, "discovery-owner.json"), d); err != nil {
		return err
	}
	if err := discoveryWriteJSON(o.Discovery.Prepare, d); err != nil {
		return err
	}
	complete = true
	fmt.Printf("prepared owned discovery descriptor %s; no daemon or store was opened\n", o.Discovery.Prepare)
	return nil
}

func discoveryDigestValid(s string, n int) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == n
}
func discoveryOwnedFile(root, path string) error {
	canonical, err := canonicalPath(path)
	if err != nil || canonical != path || !inside(root, path) || root == path {
		return errors.New("discovery lineage path is not canonical inside its owned root")
	}
	_, err = discoveryFileSHA(path)
	return err
}

// No catalog/path rewriting is allowed: the snapshot was upgraded at the same
// roots that this one-shot consumer will use. All source files stay unchanged.
func resumeDiscoveryState(ctx context.Context, o runOptions, corpora []Corpus) (*privateRun, error) {
	var l discoverySeedLineage
	if err := discoveryReadJSON(o.Discovery.Resume, &l); err != nil {
		return nil, err
	}
	if l.State != "VALIDATED_OWNED_UPGRADE" || l.SourceGeneration == "" || l.OldVersion == "" || l.SchemaVersion != 44 || !l.NormalStop || !l.OwnersExited || l.OwnerPID < 1 || !discoveryDigestValid(l.OldBinarySHA256, 32) || !discoveryDigestValid(l.UpgradeBinarySHA256, 32) || !discoveryDigestValid(l.UpgradeSourceCommit, 20) {
		return nil, errors.New("discovery seed lacks complete validated upgrade lineage")
	}
	sha, err := discoveryFileSHA(l.Descriptor)
	if err != nil || sha != l.DescriptorSHA256 {
		return nil, errors.New("discovery descriptor digest mismatch")
	}
	var d discoverySeedDescriptor
	if err := discoveryReadJSON(l.Descriptor, &d); err != nil {
		return nil, err
	}
	scratch, err := canonicalPath(o.Scratch)
	if err != nil {
		return nil, err
	}
	root, err := canonicalPath(d.Root)
	if err != nil || root != d.Root || root != l.OwnedRoot || !inside(scratch, root) || root == scratch || !strings.HasPrefix(filepath.Base(root), "load-") {
		return nil, errors.New("discovery seed is outside its exact owned scratch root")
	}
	var owner discoverySeedDescriptor
	if err := discoveryReadJSON(filepath.Join(root, "discovery-owner.json"), &owner); err != nil || !reflect.DeepEqual(owner, d) {
		return nil, errors.New("discovery root ownership does not match descriptor")
	}
	if d.State != "PREPARED_ROOTS_NO_STORE" || !discoveryDigestValid(d.Nonce, 32) || d.PrimaryCount != 2 || d.LinkedCount < 0 || d.NestedCount < 0 || d.NestedCount > d.LinkedCount || d.TotalCount != 2+d.LinkedCount || len(d.Repos) != d.TotalCount || !reflect.DeepEqual(l.Repos, d.Repos) {
		return nil, errors.New("discovery lineage topology/identity differs from preparation")
	}
	if len(corpora) != 2 || !reflect.DeepEqual(d.Repos[0].Repo.Corpus, corpora[0]) || !reflect.DeepEqual(d.Repos[1].Repo.Corpus, corpora[1]) {
		return nil, errors.New("discovery seed corpus pins mismatch")
	}
	for path, want := range map[string]string{filepath.Join(root, "config.yaml"): d.ConfigSHA256, filepath.Join(root, "config", "gortex", "config.yaml"): d.BootConfigSHA256, filepath.Join(root, "gitconfig"): d.GitConfigSHA256, l.StopReceipt: l.StopReceiptSHA256, l.IdentityReceipt: l.IdentityReceiptSHA256} {
		if err := discoveryOwnedFile(root, path); err != nil {
			return nil, err
		}
		actual, err := discoveryFileSHA(path)
		if err != nil || !discoveryDigestValid(want, 32) || actual != want {
			return nil, errors.New("discovery configuration/producer proof digest mismatch")
		}
	}
	for _, proofPath := range []string{l.StopReceipt, l.IdentityReceipt} {
		var proof discoverySeedProof
		if err := discoveryReadJSON(proofPath, &proof); err != nil {
			return nil, err
		}
		if proof.State != "OBSERVED_OWNED_UPGRADE" || proof.ObservedAt.IsZero() || proof.OwnedRoot != root || proof.OwnerPID != l.OwnerPID || !proof.NormalStop || !proof.OwnersExited || proof.MainSHA256 != l.MainSHA256 || proof.WALSHA256 != l.WALSHA256 || proof.SchemaVersion != 44 || proof.UpgradeBinarySHA256 != l.UpgradeBinarySHA256 || !reflect.DeepEqual(proof.Repos, d.Repos) {
			return nil, errors.New("discovery producer proof is unknown or does not bind this stopped snapshot and identity topology")
		}
	}
	process, err := os.FindProcess(l.OwnerPID)
	if err != nil {
		return nil, err
	}
	err = process.Signal(syscall.Signal(0))
	_ = process.Release()
	if !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrProcessDone) {
		return nil, errors.New("discovery upgrade owner is live or cannot be proven exited")
	}
	binaryPath, err := filepath.Abs(o.Binary)
	if err != nil {
		return nil, err
	}
	p := &privateRun{Root: root, Store: filepath.Join(root, "store.db"), Config: filepath.Join(root, "config.yaml"), Binary: binaryPath}
	for _, entry := range d.Repos {
		repo := entry.Repo
		if err := discoveryOwnedFile(root, repo.File); err != nil {
			return nil, err
		}
		if !inside(root, repo.Path) || repo.File != filepath.Join(repo.Path, repo.Corpus.File) {
			return nil, errors.New("discovery repository path mismatch")
		}
		actual, err := discoveryFileSHA(repo.File)
		if err != nil || actual != entry.FileSHA256 {
			return nil, errors.New("discovery prepared source content changed")
		}
		p.Repos = append(p.Repos, repo)
	}
	p.Env = privateEnv(root, "", filepath.Join(root, "gitconfig"))
	for i, repo := range p.Repos {
		head, err := p.git(ctx, repo.Path, "rev-parse", "HEAD")
		if err != nil || strings.TrimSpace(string(head)) != repo.Corpus.Commit {
			return nil, errors.New("discovery prepared checkout HEAD changed")
		}
		common, err := p.git(ctx, repo.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil || strings.TrimSpace(string(common)) != d.Repos[i].CommonDir {
			return nil, errors.New("discovery prepared Git family changed")
		}
	}
	if err := discoveryOwnedFile(root, l.Snapshot); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Lstat(p.Store + suffix); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("discovery destination store must be absent before one-shot resume")
		}
	}
	before, err := discoverySeedPair(l.Snapshot)
	if err != nil || before[0] != l.MainSHA256 || before[1] != l.WALSHA256 {
		return nil, errors.New("discovery quiescent snapshot digest mismatch")
	}
	f, err := os.Open(l.Snapshot)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 100)
	_, readErr := io.ReadFull(f, header)
	err = errors.Join(readErr, f.Close())
	if err != nil || string(header[:16]) != "SQLite format 3\x00" || binary.BigEndian.Uint32(header[60:64]) != 44 {
		return nil, errors.New("discovery snapshot is not a checkpointed schema44 SQLite main")
	}
	if err := discoveryWriteJSON(filepath.Join(root, "discovery-consumed.json"), map[string]string{"descriptor_sha256": sha, "upgrade_binary_sha256": l.UpgradeBinarySHA256}); err != nil {
		return nil, err
	}
	if err := copySeedStore(l.Snapshot, p.Store); err != nil {
		return nil, err
	}
	after, err := discoverySeedPair(l.Snapshot)
	if err != nil || before != after {
		return nil, errors.New("discovery snapshot changed during copy; partial destination retained")
	}
	copied, err := discoverySeedPair(p.Store)
	if err != nil || copied != before {
		return nil, errors.New("discovery seed copy differs; partial destination retained")
	}
	p.SocketDir, err = os.MkdirTemp("/tmp", "gl-")
	if err != nil {
		return nil, err
	}
	p.Socket = filepath.Join(p.SocketDir, "d.sock")
	p.Env = privateEnv(root, p.Socket, filepath.Join(root, "gitconfig"))
	return p, nil
}
func discoverySeedPair(path string) ([2]string, error) {
	var out [2]string
	var err error
	if _, err := os.Lstat(path + "-shm"); !errors.Is(err, os.ErrNotExist) {
		return out, errors.New("discovery snapshot has shared-memory state or unknown ownership")
	}
	out[0], err = discoveryFileSHA(path)
	if err != nil {
		return out, err
	}
	out[1], err = discoveryFileSHA(path + "-wal")
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return out, err
}

// Both retained producer receipts must bind observations to this exact stopped
// output; state assertions without snapshot and identity evidence are refused.
type discoverySeedProof struct {
	State               string              `json:"state"`
	ObservedAt          time.Time           `json:"observed_at"`
	OwnedRoot           string              `json:"owned_root"`
	OwnerPID            int                 `json:"owner_pid"`
	NormalStop          bool                `json:"normal_stop"`
	OwnersExited        bool                `json:"owners_exited"`
	MainSHA256          string              `json:"main_sha256"`
	WALSHA256           string              `json:"wal_sha256"`
	SchemaVersion       int                 `json:"schema_version"`
	UpgradeBinarySHA256 string              `json:"upgrade_binary_sha256"`
	Repos               []discoverySeedRepo `json:"repositories"`
}
