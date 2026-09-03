//go:build darwin && cgo

package fswatcher

/*
#cgo LDFLAGS: -framework CoreServices -framework Foundation
#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <stdio.h>

// Go implementation must treat this data as read-only
extern void goHandleFSEvent(
    uintptr_t handle,
    size_t numEvents,
    char** paths,
    FSEventStreamEventFlags* flags,
    FSEventStreamEventId* ids
);

// Callback to log stream creation
extern void goLogStreamSuccess(
    uintptr_t handle,
    char* path
);

// Forwards event data to the Go handler
static void fsEventCallback(
    ConstFSEventStreamRef streamRef,
    void *clientCallBackInfo,
    size_t numEvents,
    void *eventPaths,
    const FSEventStreamEventFlags eventFlags[],
    const FSEventStreamEventId eventIds[]
) {
    if (numEvents == 0) {
        return;
    }
    // Cast void pointers to their concrete types
    goHandleFSEvent(
        (uintptr_t)clientCallBackInfo,
        numEvents,
        (char**)eventPaths,
        (FSEventStreamEventFlags*)eventFlags,
        (FSEventStreamEventId*)eventIds
    );
}

// Creates and schedules an FSEventStream on a dedicated dispatch queue
static FSEventStreamRef createAndScheduleStream(
    const char *path,
    uintptr_t handle,
    double latency,
    int debug
) {
    CFStringRef pathRef = CFStringCreateWithCString(NULL, path, kCFStringEncodingUTF8);
    if (!pathRef) {
        if (debug) fprintf(stderr, "fswatcher: C ERROR - Unable to create CFString for path: %s\n", path);
        return NULL;
    }

    CFArrayRef pathsToWatch = CFArrayCreate(NULL, (const void **)&pathRef, 1, &kCFTypeArrayCallBacks);
    CFRelease(pathRef);
    if (!pathsToWatch) {
        if (debug) fprintf(stderr, "fswatcher: C ERROR - Unable to create CFArray for path\n");
        return NULL;
    }

    FSEventStreamContext context = {0, (void*)handle, NULL, NULL, NULL};

    FSEventStreamRef stream = FSEventStreamCreate(
        NULL,
        (FSEventStreamCallback)fsEventCallback,
        &context,
        pathsToWatch,
        kFSEventStreamEventIdSinceNow,
        (CFTimeInterval)latency,
        kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagNoDefer | kFSEventStreamCreateFlagWatchRoot
    );
    CFRelease(pathsToWatch);

    if (!stream) {
        if (debug) fprintf(stderr, "fswatcher: C ERROR - Unable to create FSEventStream\n");
        return NULL;
    }

    dispatch_queue_t queue = dispatch_queue_create("com.fswatcher.fsevents", DISPATCH_QUEUE_SERIAL);
    if (!queue) {
        if (debug) fprintf(stderr, "fswatcher: C ERROR - Unable to create dispatch queue\n");
        FSEventStreamRelease(stream);
        return NULL;
    }

    FSEventStreamSetDispatchQueue(stream, queue);
    dispatch_release(queue); // The stream retains the queue

    if (debug) goLogStreamSuccess(handle, (char*)path);
    return stream;
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"
)

var (
	watcherMap sync.Map
)

// FSEvents flag constants, mirroring C definitions
const (
	FSEventStreamEventFlagNone               uint32 = 0x00000000
	FSEventStreamEventFlagMustScanSubDirs    uint32 = 0x00000001
	FSEventStreamEventFlagUserDropped        uint32 = 0x00000002
	FSEventStreamEventFlagKernelDropped      uint32 = 0x00000004
	FSEventStreamEventFlagEventIdsWrapped    uint32 = 0x00000008
	FSEventStreamEventFlagHistoryDone        uint32 = 0x00000010
	FSEventStreamEventFlagRootChanged        uint32 = 0x00000020
	FSEventStreamEventFlagMount              uint32 = 0x00000040
	FSEventStreamEventFlagUnmount            uint32 = 0x00000080
	FSEventStreamEventFlagOwnEvent           uint32 = 0x00080000
	FSEventStreamEventFlagItemCreated        uint32 = 0x00000100
	FSEventStreamEventFlagItemRemoved        uint32 = 0x00000200
	FSEventStreamEventFlagItemInodeMetaMod   uint32 = 0x00000400
	FSEventStreamEventFlagItemRenamed        uint32 = 0x00000800
	FSEventStreamEventFlagItemModified       uint32 = 0x00001000
	FSEventStreamEventFlagItemFinderInfoMod  uint32 = 0x00002000
	FSEventStreamEventFlagItemChangeOwner    uint32 = 0x00004000
	FSEventStreamEventFlagItemXattrMod       uint32 = 0x00008000
	FSEventStreamEventFlagItemIsFile         uint32 = 0x00010000
	FSEventStreamEventFlagItemIsDir          uint32 = 0x00020000
	FSEventStreamEventFlagItemIsSymlink      uint32 = 0x00040000
	FSEventStreamEventFlagItemIsHardlink     uint32 = 0x00100000
	FSEventStreamEventFlagItemIsLastHardlink uint32 = 0x00200000
	FSEventStreamEventFlagItemCloned         uint32 = 0x00400000
)

// flagInfo holds the value and name for an FSEvent flag
type flagInfo struct {
	value uint32
	name  string
}

// knownFlags maps FSEvent flag values to their string names
var knownFlags = []flagInfo{
	{FSEventStreamEventFlagMustScanSubDirs, "MustScanSubDirs"},
	{FSEventStreamEventFlagUserDropped, "UserDropped"},
	{FSEventStreamEventFlagKernelDropped, "KernelDropped"},
	{FSEventStreamEventFlagEventIdsWrapped, "EventIdsWrapped"},
	{FSEventStreamEventFlagHistoryDone, "HistoryDone"},
	{FSEventStreamEventFlagRootChanged, "RootChanged"},
	{FSEventStreamEventFlagMount, "Mount"},
	{FSEventStreamEventFlagUnmount, "Unmount"},
	{FSEventStreamEventFlagOwnEvent, "OwnEvent"},
	{FSEventStreamEventFlagItemIsFile, "IsFile"},
	{FSEventStreamEventFlagItemIsDir, "IsDir"},
	{FSEventStreamEventFlagItemIsSymlink, "IsSymlink"},
	{FSEventStreamEventFlagItemIsHardlink, "IsHardlink"},
	{FSEventStreamEventFlagItemIsLastHardlink, "IsLastHardlink"},
	{FSEventStreamEventFlagItemCreated, "Created"},
	{FSEventStreamEventFlagItemRemoved, "Removed"},
	{FSEventStreamEventFlagItemRenamed, "Renamed"},
	{FSEventStreamEventFlagItemModified, "Modified"},
	{FSEventStreamEventFlagItemInodeMetaMod, "InodeMetaMod"},
	{FSEventStreamEventFlagItemFinderInfoMod, "FinderInfoMod"},
	{FSEventStreamEventFlagItemChangeOwner, "ChangeOwner"},
	{FSEventStreamEventFlagItemXattrMod, "XattrMod"},
	{FSEventStreamEventFlagItemCloned, "Cloned"},
}

// parseDarwinEventFlags converts raw FSEvent flags into a slice of human-readable strings
func parseDarwinEventFlags(flags uint32) []string {
	if flags == FSEventStreamEventFlagNone {
		return nil
	}
	var descriptions []string
	for _, info := range knownFlags {
		if flags&info.value != 0 {
			descriptions = append(descriptions, info.name)
		}
	}
	return descriptions
}

// parseGenericEventFlags maps Darwin-specific flags to our cross-platform EventTypes
func parseGenericEventFlags(flags uint32) []EventType {
	var events []EventType
	if flags&FSEventStreamEventFlagItemCreated != 0 {
		events = append(events, EventCreate)
	}
	if flags&FSEventStreamEventFlagItemRemoved != 0 {
		events = append(events, EventRemove)
	}
	if flags&FSEventStreamEventFlagItemRenamed != 0 || flags&FSEventStreamEventFlagRootChanged != 0 {
		events = append(events, EventRename)
	}
	if flags&(FSEventStreamEventFlagItemModified|FSEventStreamEventFlagItemInodeMetaMod|FSEventStreamEventFlagItemFinderInfoMod|FSEventStreamEventFlagItemXattrMod) != 0 {
		events = append(events, EventMod)
	}
	if flags&FSEventStreamEventFlagItemChangeOwner != 0 {
		events = append(events, EventChmod)
	}
	return uniqueEventTypes(events)
}

// goHandleFSEvent is the CGo callback that processes events from the FSEvents API
//
//export goHandleFSEvent
func goHandleFSEvent(handle C.uintptr_t, numEvents C.size_t, cPaths **C.char, cFlags *C.FSEventStreamEventFlags, cIds *C.FSEventStreamEventId) {
	watcherPtr, ok := watcherMap.Load(uintptr(handle))
	if !ok {
		return // Watcher was unregistered, ignore event
	}
	w := watcherPtr.(*watcher)

	// Avoid processing stale events during shutdown
	if w.isShuttingDown.Load() {
		return
	}

	// Create Go slices that point to the C array data without copying
	count := int(numEvents)
	pathsSlice := unsafe.Slice(cPaths, count)
	flagsSlice := unsafe.Slice(cFlags, count)
	idsSlice := unsafe.Slice(cIds, count)

	currentTime := time.Now()

	for i, cPath := range pathsSlice {
		// Copy C data into Go memory
		path := C.GoString(cPath)
		flags := uint32(flagsSlice[i])
		eventID := uint64(idsSlice[i])

		// Handle MustScanSubDirs and dropped events
		if flags&(FSEventStreamEventFlagMustScanSubDirs|FSEventStreamEventFlagUserDropped|FSEventStreamEventFlagKernelDropped) != 0 {
			w.logWarn("FSEvents signaling potential data loss", "flags", fmt.Sprintf("%08x", flags), "path", path)
			go w.scanDirectory(path)
			continue
		}

		genericTypes := parseGenericEventFlags(flags)
		if len(genericTypes) == 0 {
			continue // Skip events we can't map to a generic type
		}

		event := WatchEvent{
			ID:    eventID,
			Path:  path,
			Types: genericTypes,
			Flags: parseDarwinEventFlags(flags),
			Time:  currentTime,
		}

		w.handlePlatformEvent(event)
	}
}

// goLogStreamSuccess logs successful stream creation
//
//export goLogStreamSuccess
func goLogStreamSuccess(handle C.uintptr_t, cPath *C.char) {
	watcherPtr, ok := watcherMap.Load(uintptr(handle))
	if !ok {
		return
	}
	w := watcherPtr.(*watcher)
	path := C.GoString(cPath)
	w.logDebug("FSEventStream created successfully", "path", path)
}

// fsEvents implements the platformNotifier interface using FSEvents
type fsEvents struct {
	w       *watcher
	streams map[string]C.FSEventStreamRef
	mu      sync.Mutex
}

// newFSEvents creates a new instance of fsEvents
func newFSEvents(w *watcher) *fsEvents {
	return &fsEvents{
		w:       w,
		streams: make(map[string]C.FSEventStreamRef),
	}
}

// startPlatform initializes the FSEvents backend for macOS
func (w *watcher) startPlatform(ctx context.Context) (<-chan struct{}, error) {
	w.handle = uintptr(unsafe.Pointer(w))
	watcherMap.Store(w.handle, w)

	fse := newFSEvents(w)
	w.streams = make(map[string]any)
	w.streams["platform"] = fse

	platformIsDone := make(chan struct{})

	go func() {
		<-ctx.Done()
		w.logDebug("Platform backend shutting down")
		watcherMap.Delete(w.handle)

		fse.mu.Lock()
		defer fse.mu.Unlock()

		for path, streamRef := range fse.streams {
			C.FSEventStreamStop(streamRef)
			C.FSEventStreamInvalidate(streamRef)
			C.FSEventStreamRelease(streamRef)
			w.logDebug("Platform backend cleaned up watch", "path", path)
		}
		fse.streams = make(map[string]C.FSEventStreamRef) // Clear the map

		close(platformIsDone)
	}()

	return platformIsDone, nil
}

// addWatch creates and starts an FSEventStream for a given path
func (f *fsEvents) addWatch(watchPath *WatchPath) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := watchPath.Path
	if _, exists := f.streams[path]; exists {
		return fmt.Errorf("path %s is already being monitored", path)
	}

	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	// Use a fixed, low latency for the FSEvents stream for responsiveness.
	// Application-level debouncing is handled separately by the watcher's cooldown.
	const streamLatency = 0.05 // 50 milliseconds
	latency := C.double(streamLatency)

	cDebug := C.int(0)
	if f.w.severity <= SeverityDebug {
		cDebug = 1
	}

	// FSEvents streams are always recursive; filtering is handled in Go
	streamRef := C.createAndScheduleStream(cPath, C.uintptr_t(f.w.handle), latency, cDebug)
	if streamRef == nil {
		return fmt.Errorf("failed to create FSEvents stream for path %s", path)
	}

	if C.FSEventStreamStart(streamRef) == 0 {
		C.FSEventStreamInvalidate(streamRef)
		C.FSEventStreamRelease(streamRef)
		return newError("startStream", path, errors.New("failed to start FSEvents stream"))
	}

	f.streams[path] = streamRef
	f.w.logInfo("Added watch", "path", path)
	return nil
}

// removeWatch stops and releases an FSEventStream for a given path
func (f *fsEvents) removeWatch(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.streams) == 0 {
		f.w.logDebug("No streams to remove")
		return nil
	}

	streamRef, ok := f.streams[path]
	if !ok {
		return fmt.Errorf("path %s is not being monitored", path)
	}

	C.FSEventStreamStop(streamRef)
	C.FSEventStreamInvalidate(streamRef)
	C.FSEventStreamRelease(streamRef)

	delete(f.streams, path)
	f.w.logInfo("Removed watch", "path", path)
	return nil
}
