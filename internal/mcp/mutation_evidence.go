package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// errEvidenceDryRun refuses the one combination that cannot mean anything: a
// dry run writes no bytes, so there is nothing on disk for a receipt to attest.
const errEvidenceDryRun = "physical_evidence requires a real write; dry_run leaves no disk bytes to attest"

// errEvidenceSecretIntent withholds a receipt whose before-digest would attest
// a config file's secret-shaped values. read_file already requires explicit
// intent to hash such a file; the mutation path must not become the way around
// that check.
const errEvidenceSecretIntent = "physical_evidence is withheld for a config file carrying secret-shaped values; " +
	"read its digest with read_file{allow_secrets:true} if you genuinely need it"

// Parameter blurbs are shared constants because the cold tools/list byte
// ceiling is measured, and three copies of the same sentence is pure tax.
const (
	mutationEvidenceParamDescription = "Return a disk-verified receipt: before_sha256/after_sha256 re-read from disk " +
		"after the write (not the buffer that was meant to be written), plus resolved path and byte count. Default: false."
	evidenceDigestParamDescription = "Digest for physical_evidence. Only sha256 is supported; defaults to sha256."
)

// sha256Hex is the canonical physical-evidence digest: raw SHA-256 over the
// exact byte buffer, lowercase hex. Deliberately NOT gitBlobSHA — that helper
// is a git blob SHA-1 drift anchor with a length-prefixed header, and the two
// must never be confused for each other on the wire.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// parsePhysicalEvidenceRequest parses the shared physical_evidence / digest
// contract. read_file and every single-file mutation handler go through this
// one function so the surfaces cannot drift into accepting different digest
// vocabularies, and so an option a handler does not implement fails loudly
// instead of being silently dropped — a caller that asked for a receipt and got
// an ordinary response back has no way to tell the difference otherwise.
func parsePhysicalEvidenceRequest(req mcp.CallToolRequest) (bool, error) {
	requested := req.GetBool("physical_evidence", false)
	digest := strings.ToLower(strings.TrimSpace(req.GetString("digest", "")))
	if digest != "" && !requested {
		return false, errors.New("digest requires physical_evidence=true")
	}
	if requested && digest != "" && digest != "sha256" {
		return false, fmt.Errorf("unsupported physical evidence digest %q; only sha256 is supported", digest)
	}
	return requested, nil
}

// guardMutationEvidenceIntent refuses a receipt that would publish a digest of
// secret-shaped config values the caller never supplied.
//
// Only the pre-write content needs the gate. after_sha256 covers bytes the
// caller itself authored, and new_sha already publishes an equivalent digest of
// exactly those bytes, so gating it would buy nothing. before_sha256 is the
// genuinely new capability: today base_sha can only confirm one guess per call,
// whereas a published digest can be attacked offline.
//
// Runs before the write, so a refusal never leaves a mutated file behind.
func (s *Server) guardMutationEvidenceIntent(language, relPath string, before []byte) error {
	if _, wouldRedact := s.maybeRedactConfigLeaf(language, relPath, false, string(before)); wouldRedact {
		return errors.New(errEvidenceSecretIntent)
	}
	return nil
}

// attachMutationPhysicalEvidence records a digest of the bytes that are on disk
// after the write, read back through the same hardened helper read_file uses,
// instead of a digest of the buffer the handler intended to write. That is the
// whole distinction the receipt is claiming: new_sha is a prediction computed
// before AtomicWriteFile, after_sha256 is an observation made after it.
//
// The write has already landed by the time this runs, so a failure here is
// reported as evidence_error on an otherwise truthful success response, never
// as a call failure — failing the call would invite a retry that applies the
// same edit twice.
func (s *Server) attachMutationPhysicalEvidence(ctx context.Context, resp map[string]any, absPath string, before []byte, beforeExisted bool) {
	read := readPhysicalFileEvidence
	if s.physicalEvidenceOverride != nil {
		read = s.physicalEvidenceOverride
	}
	_, evidence, err := read(absPath)
	if err == nil {
		// Confinement is re-checked against the target that actually supplied
		// the hashed bytes. resolveFilePath already confined absPath, but a
		// symlink can be retargeted between that resolution and this read-back,
		// and only a guard on the resolved target sees it. Same two guards, same
		// order, as the read path — a new file-touching sink that skips them is
		// exactly the shape of this project's two published advisories.
		err = s.guardResolvedPathWithinRepo(ctx, absPath, evidence.resolvedPath)
	}
	if err == nil {
		err = s.guardSymlinkWithinRepo(ctx, absPath)
	}
	if err != nil {
		resp["evidence_error"] = err.Error()
		return
	}
	if beforeExisted {
		resp["before_sha256"] = sha256Hex(before)
	} else {
		resp["before_absent"] = true
	}
	resp["after_sha256"] = evidence.contentSHA256
	resp["hash_algorithm"] = "sha256"
	resp["hash_scope"] = "full_file"
	resp["content_source"] = "disk"
	resp["disk_verified"] = true
	resp["resolved_path"] = evidence.resolvedPath
	resp["byte_count"] = evidence.byteCount
	resp["verified_at"] = evidence.readAt.Format(time.RFC3339Nano)
}
