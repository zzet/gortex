package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"time"

	"github.com/zzet/gortex/internal/indexer"
)

// These fields come from actual list_checkouts observations, never prepared
// topology declarations. View riders are intentionally not catalog identity.
type discoveryCatalogIdentity struct {
	Root        string `json:"root"`
	CheckoutID  string `json:"checkout_id"`
	Incarnation string `json:"incarnation"`
	FamilyID    string `json:"family_id"`
	RepoPrefix  string `json:"repo_prefix"`
	CommonDir   string `json:"common_dir"`
}
type discoveryCatalogScope struct {
	CWD      string                   `json:"cwd"`
	Overview indexer.FamiliesOverview `json:"overview"`
}
type discoveryCatalogReceipt struct {
	Before []discoveryCatalogScope `json:"before"`
	After  []discoveryCatalogScope `json:"after"`
}

func discoveryCatalogIdentities(scopes []discoveryCatalogScope, repos []discoverySeedRepo) (map[string]discoveryCatalogIdentity, error) {
	expected := make(map[string]discoverySeedRepo, len(repos))
	for _, repo := range repos {
		expected[repo.Repo.Path] = repo
	}
	result := make(map[string]discoveryCatalogIdentity, len(repos))
	ids := make(map[string]string)
	owners := make(map[string]string)
	for _, scope := range scopes {
		for _, family := range scope.Overview.Families {
			if family.FamilyID == "" || family.CommonDir == "" || family.PrimaryRepoPrefix == "" {
				return nil, errors.New("discovery catalog has incomplete family identity")
			}
			graphs := make(map[string]indexer.GraphOverview)
			for _, graph := range family.Graphs {
				if graph.GraphID == "" || graph.RepoPrefix == "" || graph.OwnerCheckoutID == "" {
					return nil, errors.New("discovery catalog has incomplete graph identity")
				}
				if old, ok := graphs[graph.GraphID]; ok && old != graph {
					return nil, errors.New("discovery catalog graph has conflicting identities")
				}
				graphs[graph.GraphID] = graph
				owners[family.FamilyID+"\x00"+graph.RepoPrefix] = graph.OwnerCheckoutID
			}
			for _, checkout := range family.Checkouts {
				repo, ok := expected[checkout.RootPath]
				prefix := family.PrimaryRepoPrefix
				if checkout.GraphID != "" {
					prefix = graphs[checkout.GraphID].RepoPrefix
				}
				if !ok || checkout.CheckoutID == "" || checkout.Incarnation == "" || prefix != repo.Repo.Prefix || family.CommonDir != repo.CommonDir {
					return nil, errors.New("discovery catalog checkout does not bind prepared root and family")
				}
				row := discoveryCatalogIdentity{Root: checkout.RootPath, CheckoutID: checkout.CheckoutID, Incarnation: checkout.Incarnation, FamilyID: family.FamilyID, RepoPrefix: prefix, CommonDir: family.CommonDir}
				if old, ok := result[row.Root]; ok && old != row {
					return nil, errors.New("discovery catalog root has conflicting identity")
				}
				if root, ok := ids[row.CheckoutID]; ok && root != row.Root {
					return nil, errors.New("discovery catalog ID names multiple roots")
				}
				result[row.Root] = row
				ids[row.CheckoutID] = row.Root
			}
		}
	}
	if len(result) != len(expected) {
		return nil, errors.New("discovery catalog does not cover every prepared root")
	}
	for _, repo := range repos[:2] {
		row := result[repo.Repo.Path]
		if owners[row.FamilyID+"\x00"+row.RepoPrefix] != row.CheckoutID {
			return nil, errors.New("discovery catalog primary graph has wrong checkout owner")
		}
	}
	return result, nil
}

func discoveryReadCatalogReceipt(root string, proof discoverySeedProof, repos []discoverySeedRepo) (map[string]discoveryCatalogIdentity, error) {
	if err := discoveryOwnedFile(root, proof.CatalogReceipt); err != nil {
		return nil, err
	}
	hash, err := discoveryFileSHA(proof.CatalogReceipt)
	if err != nil || !discoveryDigestValid(proof.CatalogReceiptSHA256, 32) || hash != proof.CatalogReceiptSHA256 {
		return nil, errors.New("discovery raw catalog receipt digest mismatch")
	}
	var raw discoveryCatalogReceipt
	if err := discoveryReadJSON(proof.CatalogReceipt, &raw); err != nil {
		return nil, err
	}
	before, err := discoveryCatalogIdentities(raw.Before, repos)
	if err != nil {
		return nil, err
	}
	after, err := discoveryCatalogIdentities(raw.After, repos)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(before, after) {
		return nil, errors.New("discovery old-to-upgraded catalog identity mapping changed")
	}
	return after, nil
}

func (p *privateRun) verifyDiscoveryCatalog(ctx context.Context) (json.RawMessage, error) {
	if p.discoveryCatalog == nil {
		return nil, nil
	}
	var scopes []discoveryCatalogScope
	for _, repo := range p.Repos[:2] {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		conn, reader, version, err := dialSocketSurface(callCtx, p.Socket, repo.Path, "mcp", "full", "hide")
		if err != nil {
			cancel()
			return nil, err
		}
		client := &socketClient{conn: conn, reader: reader, pending: make(map[int64]chan wireReply), done: make(chan struct{}), version: version, cwd: repo.Path, writeGate: make(chan struct{}, 1)}
		go client.readLoop()
		_, err = client.call(callCtx, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "discovery-lineage", "version": "1"}})
		if err == nil {
			err = client.send(callCtx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, false)
		}
		var raw json.RawMessage
		if err == nil {
			raw, err = client.call(callCtx, "tools/call", map[string]any{"name": "list_checkouts", "arguments": map[string]any{"format": "json", "max_bytes": 0}})
		}
		_ = client.Close()
		cancel()
		if err != nil {
			return nil, err
		}
		payload, err := textPayload(raw, "content")
		if err != nil {
			return nil, err
		}
		var overview indexer.FamiliesOverview
		if err = json.Unmarshal(payload, &overview); err != nil {
			return nil, err
		}
		scopes = append(scopes, discoveryCatalogScope{CWD: repo.Path, Overview: overview})
	}
	repos := make([]discoverySeedRepo, len(p.Repos))
	for i, repo := range p.Repos {
		identity := p.discoveryCatalog[repo.Path]
		repos[i] = discoverySeedRepo{Repo: repo, CommonDir: identity.CommonDir}
	}
	actual, err := discoveryCatalogIdentities(scopes, repos)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(actual, p.discoveryCatalog) {
		return nil, errors.New("resumed discovery changed observed catalog checkout identity")
	}
	raw, err := json.Marshal(scopes)
	if err == nil {
		err = discoveryWriteJSON(filepath.Join(p.Root, "resumed-catalog-observed.json"), scopes)
	}
	return raw, err
}
