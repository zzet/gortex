package mcp

import (
	"context"
	"path/filepath"
	"slices"

	"github.com/zzet/gortex/internal/pathkey"
)

// This capability belongs to the constructor-bound physical store, never to a
// selected graph reader. It grants no graph or checkout authority.
type interactiveReadAnnouncer interface{ AnnounceInteractiveRead() func() }

type interactiveEditedFile struct{ path, checkoutID, incarnation string }

func (s *Server) announceInteractiveRead() func() {
	if store, ok := s.graph.(interactiveReadAnnouncer); ok {
		return store.AnnounceInteractiveRead()
	}
	if s.materializer != nil && s.materializer.Store != nil {
		return s.materializer.Store.AnnounceInteractiveRead()
	}
	return func() {}
}

func interactiveFileKey(ctx context.Context, absPath string) interactiveEditedFile {
	checkout, incarnation := mutationCheckoutScope(ctx)
	if incarnation == "" {
		if view := requestViewFromContext(ctx); view != nil {
			incarnation = view.sourceCheckoutIncarnation
		}
	}
	return interactiveEditedFile{path: pathkey.MutationKey(absPath), checkoutID: checkout, incarnation: incarnation}
}

func (s *Server) rememberInteractiveEdit(ctx context.Context, absPath string) {
	if absPath == "" {
		return
	}
	file := interactiveFileKey(ctx, absPath)
	session := s.sessionFor(ctx)
	session.mu.Lock()
	defer session.mu.Unlock()
	session.interactiveEditedFiles = slices.DeleteFunc(session.interactiveEditedFiles, func(prior interactiveEditedFile) bool { return prior == file })
	session.interactiveEditedFiles = append([]interactiveEditedFile{file}, session.interactiveEditedFiles...)
	if len(session.interactiveEditedFiles) > maxSessionItems {
		session.interactiveEditedFiles = session.interactiveEditedFiles[:maxSessionItems]
	}
}

// Called after the request selected its actual reader, before its handler.
// The first matching read consumes the bounded edit identity. Matching uses
// a confined physical file plus the existing checkout identity;
// another session/checkout, ref or editor projection cannot borrow the marker.
func (s *Server) beginEditedFileRead(ctx context.Context, absPath string) func() {
	if !filepath.IsAbs(absPath) {
		return func() {}
	}
	if refViewFilesFor(ctx) != nil || OverlayViewFromContext(ctx) != nil {
		return func() {}
	}
	session := s.sessionFor(ctx)
	session.mu.Lock()
	empty := len(session.interactiveEditedFiles) == 0
	session.mu.Unlock()
	if empty {
		return func() {}
	}
	if err := s.guardSymlinkWithinRepo(ctx, absPath); err != nil {
		return func() {}
	}
	if view := sourceRequestView(ctx); view != nil && !requestViewPathRoot(ctx).contains(absPath) {
		return func() {}
	}
	if _, present, _, _ := s.sourceOverlayFile(ctx, absPath); present {
		return func() {}
	}
	if _, present := s.overlayContentFor(ctx, absPath); present {
		return func() {}
	}
	file := interactiveFileKey(ctx, absPath)
	session.mu.Lock()
	index := slices.Index(session.interactiveEditedFiles, file)
	matched := index >= 0
	if matched {
		session.interactiveEditedFiles = slices.Delete(session.interactiveEditedFiles, index, index+1)
	}
	session.mu.Unlock()
	if !matched {
		return func() {}
	}
	return s.announceInteractiveRead()
}

// Begin only after publication/repair and response graph reads have finished.
// No store mutation or publication wait may run within this formatting scope.
func (s *Server) beginEditAcknowledgement(ctx context.Context, absPath string) func() {
	s.rememberInteractiveEdit(ctx, absPath)
	return s.announceInteractiveRead()
}
