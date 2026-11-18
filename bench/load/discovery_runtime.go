package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

const discoveryRuntimeMarker = "discovery-runtime-owner.json"

type discoveryRuntimeIdentity struct {
	Root             string `json:"runtime_root"`
	SourceRoot       string `json:"source_owner_root"`
	SourceNonce      string `json:"source_owner_nonce"`
	DescriptorSHA256 string `json:"descriptor_sha256"`
	Nonce            string `json:"runtime_nonce"`
}

// The portable lease deliberately uses os.SameFile rather than platform inode
// fields: discovery resume is Unix-only, but privateRun builds on every GOOS.
// Its root-level marker survives normal state cleanup and prevents path reuse.
type discoveryRuntimeLease struct {
	identity  discoveryRuntimeIdentity
	directory os.FileInfo
}

func (l *discoveryRuntimeLease) validate(root string) error {
	if l == nil {
		return errors.New("missing discovery runtime lease")
	}
	canonical, err := canonicalPath(root)
	if err != nil || root != l.identity.Root || canonical != root || root == l.identity.SourceRoot || inside(l.identity.SourceRoot, root) || inside(root, l.identity.SourceRoot) {
		return errors.New("refused cleanup outside the leased discovery runtime root")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(l.directory, info) {
		return errors.New("refused cleanup through a replaced discovery runtime directory")
	}
	marker := filepath.Join(root, discoveryRuntimeMarker)
	if err := discoveryOwnedFile(root, marker); err != nil {
		return err
	}
	var actual discoveryRuntimeIdentity
	if err := discoveryReadJSON(marker, &actual); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, l.identity) {
		return errors.New("refused discovery runtime ownership/nonce mismatch")
	}
	return nil
}

// Allocate only after the source descriptor, lineage, original paths and strict
// checkpointed schema44/no-SHM snapshot have all passed existing validation.
// No source directory or repository is renamed, rewritten or owned for cleanup.
func newDiscoveryRuntimeRoot(scratch, requested string, d discoverySeedDescriptor, descriptorSHA string) (*discoveryRuntimeLease, error) {
	if !discoveryDigestValid(d.Nonce, 32) || !discoveryDigestValid(descriptorSHA, 32) {
		return nil, errors.New("discovery runtime requires validated source nonce and descriptor digest")
	}
	root, err := canonicalPath(requested)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(requested) || root != filepath.Clean(requested) || filepath.Dir(root) != scratch || root == d.Root || inside(d.Root, root) || inside(root, d.Root) || !strings.HasPrefix(filepath.Base(root), "load-") {
		return nil, errors.New("discovery runtime must be a fresh canonical sibling directly under scratch, separate from source owner")
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("discovery runtime destination must be absent, not reused or symlinked")
	}
	if err := os.Mkdir(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("discovery runtime directory changed during allocation")
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	lease := &discoveryRuntimeLease{identity: discoveryRuntimeIdentity{Root: root, SourceRoot: d.Root, SourceNonce: d.Nonce, DescriptorSHA256: descriptorSHA, Nonce: hex.EncodeToString(token)}, directory: info}
	if err := discoveryWriteJSON(filepath.Join(root, discoveryRuntimeMarker), lease.identity); err != nil {
		return nil, err
	}
	if err := lease.validate(root); err != nil {
		return nil, err
	}
	for _, dir := range []string{"home", "cache", "config", "config/gortex", "data", "runtime", "tmp", "repos", "hooks"} {
		if err := os.Mkdir(filepath.Join(root, filepath.FromSlash(dir)), 0700); err != nil {
			return nil, err
		}
	}
	for _, file := range []struct{ relative, sha string }{
		{"config.yaml", d.ConfigSHA256}, {"gitconfig", d.GitConfigSHA256}, {"config/gortex/config.yaml", d.BootConfigSHA256},
	} {
		relative := filepath.FromSlash(file.relative)
		if err := copyDiscoveryRuntimeConfig(filepath.Join(d.Root, relative), filepath.Join(root, relative), file.sha); err != nil {
			return nil, err
		}
	}
	if err := lease.validate(root); err != nil {
		return nil, err
	}
	return lease, nil
}

func copyDiscoveryRuntimeConfig(source, dest, want string) error {
	before, err := discoveryFileSHA(source)
	if err != nil || before != want {
		return errors.New("discovery source configuration changed before runtime copy")
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		_ = in.Close()
		return err
	}
	_, copyErr := io.Copy(out, in)
	if err := errors.Join(copyErr, in.Close(), out.Close()); err != nil {
		return err
	}
	after, err := discoveryFileSHA(source)
	if err != nil {
		return err
	}
	copied, err := discoveryFileSHA(dest)
	if err != nil {
		return err
	}
	if before != after || copied != before {
		return fmt.Errorf("discovery configuration copy changed: %s", filepath.Base(source))
	}
	return nil
}
