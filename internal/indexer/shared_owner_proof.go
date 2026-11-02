package indexer

import (
 "context"
 "path/filepath"
 "sort"
 "sync/atomic"
 "time"
 "weak"
)

// SharedPhysicalOwner is original physical metadata captured before admission.
// It grants no graph authority. MCP must also verify its current live ledger.
type SharedPhysicalOwner struct {
 Server string
 PID int
 Birth string
 Receipt string
 Sequence uint64
 Hash string
 Path string
 Checkout string
 Incarnation string
}

func (p *PublicationPhaseRecord) BindSharedPhysicalOwner(v SharedPhysicalOwner) {
 p.mu.Lock(); defer p.mu.Unlock()
 if p.sharedPhysical != nil || p.canonicalBound || p.terminal || v.Server == "" || v.PID <= 0 || v.Birth == "" || v.Receipt != p.key || v.Sequence == 0 || len(v.Hash) != 64 || !filepath.IsAbs(v.Path) || filepath.Clean(v.Path) != v.Path || v.Checkout != p.checkoutID { return }
 copy := v; p.sharedPhysical = &copy
}

type sharedSetInvocationKey struct{}
type sharedSetInvocation struct {
 entered time.Time
 forward weak.Pointer[Watcher]
 claimed atomic.Bool
 invalid atomic.Bool
 arms atomic.Uint64
 count int
 root, prefix string
}

func newSharedSetInvocation(at time.Time, count int) *sharedSetInvocation { return &sharedSetInvocation{entered:at,count:count} }
func sharedSetInvocationFor(ctx context.Context, w *Watcher, entered time.Time, count int) *sharedSetInvocation {
 if v,ok := ctx.Value(sharedSetInvocationKey{}).(*sharedSetInvocation); ok && v.forward.Value()==w && v.claimed.CompareAndSwap(false,true) { return v }
 return newSharedSetInvocation(entered,count)
}

type sharedOwnerInterval struct { Phase string; Start, End int64 }
type sharedOwnerMember struct {
 record weak.Pointer[PublicationPhaseRecord]
 key, path string
 ticket uint64
 physical SharedPhysicalOwner
}
type sharedOwnerFrozen struct {
 members []sharedOwnerMember
 recorder weak.Pointer[PublicationPhaseRecorder]
 recorderName string
 invocation *sharedSetInvocation
 arm uint64
 retry int
 execution canonicalAttemptWindow
 intervals []sharedOwnerInterval
 optionalDispositions map[string]string
 callbackEntry, executionReturn, drainReturn time.Time
 timerStart, timerEnd time.Time
 backgroundReason string
}

// SharedOwnerProjection carries scalar observations, never a reusable live
// membership authority. Each call revalidates every weak member and anchor.
type SharedOwnerProjection struct {
 Definition map[string]any
 Owner map[string]any
 Member map[string]any
 PhysicalMembers []SharedPhysicalOwner
 Ticket uint64
 originals []sharedOwnerMember
}

func (w *Watcher) prepareSharedOwnerAttempt(a *mutationSetPublicationAttempt) {
 a.proofFailure="missing_physical_member"
 if len(a.members)==0 || len(a.members)!=a.executorFiles { return }
 call:=a.members[0].invocation
 if call==nil || call.invalid.Load() || call.count!=len(a.members) { return }
 var recorder *PublicationPhaseRecorder
 for _,m:=range a.members {
  if m.invocation!=call { return }
  r:=m.record.owner.Value()
  if r==nil || r.sharedSelf.Value()!=r || r.sharedInstance=="" || recorder!=nil && r!=recorder { return }
  recorder=r
  m.record.mu.Lock(); physical:=m.record.sharedPhysical; valid:=physical!=nil && physical.Path==m.path && physical.Checkout=="" && physical.Incarnation==""; m.record.mu.Unlock()
  if !valid { return }
 }
 a.proofFailure=""
 a.sharedInvocation=call
 // Same existing ordered collector; no allocation on a point attempt.
 a.stamps.patchLaps.sharedIntervalLimit=len(a.members)*int(canonicalPatchLapCount)*8
 a.stamps.patchLaps.sharedIntervals=make([]sharedOwnerInterval,0,int(canonicalPatchLapCount)*2)
}

func (w *Watcher) finishSharedOwnerProof(a *mutationSetPublicationAttempt, timer *mutationSetTimerObservation, returned time.Time) {
 if a==nil { return }
 attached:=false
 reason:=a.proofFailure
 if reason=="" {reason="unsupported_invocation"}
 defer func(){if !attached {a.publishSharedOwnerUnavailable(reason)}}()
 if a.sharedInvocation==nil || a.sharedInvocation.invalid.Load() || a.retry!=0 || a.executionReturned.IsZero() || returned.Before(a.executionReturned) || timer==nil || timer.watcher.Value()!=w || timer.invocation!=a.sharedInvocation || timer.invalid.Load() || a.sharedInvocation.arms.Load()!=1 { return }
 call:=a.sharedInvocation
 if a.executorFiles!=call.count || len(a.members)!=call.count { return }
 a.stamps.mu.Lock()
 if a.stamps.patchLaps.sharedIntervalLimit<=0 || !a.stamps.patchLaps.sharedDirectEntry { a.stamps.mu.Unlock(); return }
 intervals:=append([]sharedOwnerInterval(nil),a.stamps.patchLaps.sharedIntervals...)
 dispositions:=make(map[string]string,len(a.stamps.patchLaps.sharedOptionalDispositions));for name,reason:=range a.stamps.patchLaps.sharedOptionalDispositions {dispositions[name]=reason}
 a.stamps.mu.Unlock()
 members:=make([]sharedOwnerMember,0,len(a.members))
 var owner *PublicationPhaseRecorder
 for _,m:=range a.members {
  r:=m.record.owner.Value(); if r==nil || owner!=nil && owner!=r { return }; owner=r
  m.record.mu.Lock(); physical:=m.record.sharedPhysical
  if physical==nil { m.record.mu.Unlock(); return }
  copy:=*physical; m.record.mu.Unlock()
  members=append(members,sharedOwnerMember{record:weak.Make(m.record),key:m.record.Key(),path:m.path,ticket:m.generation,physical:copy})
 }
 sort.Slice(members,func(i,j int)bool{return members[i].path<members[j].path})
 if owner==nil || owner.sharedSelf.Value()!=owner || owner.sharedInstance=="" { return }
 first:=members[0].physical
 for i,m:=range members {
  if m.physical.Server!=first.Server || m.physical.PID!=first.PID || m.physical.Birth!=first.Birth || m.physical.Checkout!="" || m.physical.Incarnation!="" || i>0 && (m.path==members[i-1].path || m.key==members[i-1].key) { return }
 }
 frozen:=&sharedOwnerFrozen{members:members,recorder:weak.Make(owner),recorderName:owner.sharedInstance,invocation:call,arm:timer.frontier,retry:a.retry,execution:a.sharedWindow,intervals:intervals,optionalDispositions:dispositions,callbackEntry:a.entered,executionReturn:a.executionReturned,drainReturn:returned,timerStart:timer.armStarted,timerEnd:timer.armFinished}
 if timer.armStarted.Before(call.entered) || timer.armFinished.Before(timer.armStarted) || a.entered.Before(timer.armFinished) { frozen.backgroundReason="overlapping_or_invalid_arm" }
 owner.mu.Lock(); defer owner.mu.Unlock()
 for _,m:=range members {
  r:=m.record.Value(); if r==nil || owner.byKey[m.key]!=r { return }
  r.mu.Lock(); valid:=!r.sharedProofRevoked && r.key==m.key && r.ticket==m.ticket && r.sharedSetExecution!=nil && r.sharedSetExecution.generation==m.ticket && r.sharedPhysical!=nil && *r.sharedPhysical==m.physical
  for _,mark:=range r.marks { if mark.phase==PublicationTicketFailed { valid=false } }
  r.mu.Unlock(); if !valid { return }
 }
 for _,m:=range members {
  r:=m.record.Value(); r.mu.Lock(); copy:=*r.sharedSetExecution; copy.ownerProof=frozen;copy.ownerProofState="frozen"; r.sharedSetExecution=&copy; r.mu.Unlock()
 }
 attached=true
}

func sharedMemberWire(m sharedOwnerMember) map[string]any { return map[string]any{"receipt":m.physical.Receipt,"publication_key":m.key,"path":m.path,"ticket":m.ticket,"physical_commit_sequence":m.physical.Sequence,"content_sha256":m.physical.Hash,"checkout_id":m.physical.Checkout,"checkout_incarnation":m.physical.Incarnation} }
func (f *sharedOwnerFrozen) ownerWire() map[string]any {
 a:=f.members[0]
 return map[string]any{"process":map[string]any{"pid":a.physical.PID,"birth_token":a.physical.Birth},"server_instance":a.physical.Server,"recorder_instance":f.recorderName,"checkout":map[string]any{"kind":"watcher_primary","root":f.invocation.root,"id":a.physical.Checkout,"incarnation":a.physical.Incarnation,"repo_prefix":f.invocation.prefix},"anchor_publication_key":a.key,"anchor_ticket":a.ticket,"retry":f.retry,"invocation_anchor_key":a.key,"invocation_anchor_ticket":a.ticket,"arm_frontier":f.arm}
}
func intervalWire(phase string,start,end int64) map[string]any { return map[string]any{"phase":phase,"start_ns":start,"end_ns":end} }
func (f *sharedOwnerFrozen) definitionWire() map[string]any {
 accounting:=f.execution.projectAccountingScope(true)
 phases:=make(map[string]any,len(accounting.PhasesMS));states:=make(map[string]string,len(accounting.PhaseStates))
 for name,value:=range accounting.PhasesMS { phases[name]=value;states[name]=accounting.PhaseStates[name] }
 complete:=accounting.PartitionComplete
 dispositions:=make(map[string]string,len(f.optionalDispositions))
 for _,name:=range []string{"patch_analysis","batch_admission"} {
  reason:=f.optionalDispositions[name]
  expected:="resolution_catchup_not_entered";if name=="batch_admission"{expected="no_batch_gate_or_hook"}
  if phases[name]==nil && reason==expected {states[name]="not_applicable";dispositions[name]=reason} else if phases[name]==nil || reason!="" {complete=false}
 }
 intervals:=make([]map[string]any,0,len(f.intervals));for _,v:=range f.intervals { intervals=append(intervals,intervalWire(v.Phase,v.Start,v.End)) }
 execution:=map[string]any{"entry_disposition":"direct_shared_set","clock":"monotonic","origin":"actual_callback_entry","start_ns":int64(0),"end_ns":f.execution.endNS,"observed_partition_complete":complete,"phases_ms":phases,"phase_states":states,"phase_dispositions":dispositions,"intervals":intervals}
 members:=make([]map[string]any,0,len(f.members));for _,m:=range f.members { members=append(members,sharedMemberWire(m)) }
 var background any
 if f.backgroundReason=="" {
  origin:=f.invocation.entered; t0:=f.timerStart.Sub(origin).Nanoseconds();t1:=f.timerEnd.Sub(origin).Nanoseconds();e0:=f.callbackEntry.Sub(origin).Nanoseconds();e1:=f.executionReturn.Sub(origin).Nanoseconds();c1:=f.drainReturn.Sub(origin).Nanoseconds()
  background=map[string]any{"clock":"monotonic","origin":"actual_original_set_scheduler_entry","start_ns":int64(0),"end_ns":c1,"end_boundary":"drain_return","excluded_work":[]string{"owner_proof_projection","outer_timer_work_done"},"observed_partition_complete":complete,"local_intervals":[]map[string]any{intervalWire("scheduling_control",0,t0),intervalWire("timer_arm",t0,t1),intervalWire("quiet_wait",t1,e0)},"execution_ref":map[string]any{"owner":f.ownerWire(),"window":"execution","start_ns":e0,"end_ns":e1},"post_execution_control":map[string]any{"start_ns":e1,"end_ns":c1}}
 }
 var reason any;if f.backgroundReason!=""{reason=f.backgroundReason}
 return map[string]any{"scope":"shared_mutation_set_executor","owner":f.ownerWire(),"frontier_count":len(f.members),"members":members,"execution":execution,"background":background,"background_unavailable_reason":reason}
}

func (p *PublicationPhaseRecord) SharedOwnerProof(current *PublicationPhaseRecorder) *SharedOwnerProjection {
 var out *SharedOwnerProjection
 p.WithSharedOwnerProof(current,func(v *SharedOwnerProjection)bool{out=v;return true})
 return out
}

func (p *PublicationPhaseRecord) WithSharedOwnerProof(current *PublicationPhaseRecorder, visit func(*SharedOwnerProjection) bool) bool {
 p.mu.Lock(); shared:=p.sharedSetExecution; var f *sharedOwnerFrozen;if shared!=nil{f=shared.ownerProof};p.mu.Unlock()
 if f==nil || current==nil || f.recorder.Value()!=current || current.sharedSelf.Value()!=current || current.sharedInstance!=f.recorderName || f.invocation.invalid.Load() { return false }
 current.mu.Lock();defer current.mu.Unlock()
 var selected *sharedOwnerMember
 physical:=make([]SharedPhysicalOwner,0,len(f.members))
 for i:=range f.members {
  m:=&f.members[i];r:=m.record.Value();if r==nil || current.byKey[m.key]!=r || r.owner.Value()!=current {return false}
  r.mu.Lock();valid:=!r.sharedProofRevoked && r.key==m.key && r.ticket==m.ticket && r.sharedPhysical!=nil && *r.sharedPhysical==m.physical && r.sharedSetExecution!=nil && r.sharedSetExecution.ownerProof==f && r.terminal
  for _,mark:=range r.marks{if mark.phase==PublicationTicketFailed{valid=false}}
  r.mu.Unlock();if !valid{return false};if r==p{selected=m};physical=append(physical,m.physical)
 }
 if selected==nil{return false}
 return visit(&SharedOwnerProjection{Definition:f.definitionWire(),Owner:f.ownerWire(),Member:sharedMemberWire(*selected),PhysicalMembers:physical,Ticket:selected.ticket,originals:f.members})
}

func (a *mutationSetPublicationAttempt) publishSharedOwnerUnavailable(reason string) {
 for _,m:=range a.members {
  m.record.mu.Lock()
  if prior:=m.record.sharedSetExecution;prior!=nil && prior.ownerProofState=="" { copy:=*prior;copy.ownerProofState="unavailable";copy.ownerProofReason=reason;m.record.sharedSetExecution=&copy }
  m.record.mu.Unlock()
 }
}

func (p *PublicationPhaseRecord) SharedOwnerProofAvailability() (string,string) {
 p.mu.Lock();defer p.mu.Unlock()
 if p.sharedSetExecution==nil {return "",""}
 if p.sharedSetExecution.ownerProofState!="" {return p.sharedSetExecution.ownerProofState,p.sharedSetExecution.ownerProofReason}
 return "pending",""
}

func (p *SharedOwnerProjection) MatchesPublication(receipt string, publication *PublicationPhaseRecord, applied uint64) bool {
 for _,m:=range p.originals {if m.physical.Receipt==receipt {return m.record.Value()==publication && m.ticket==applied}}
 return false
}

func (s *PublicationStamps) markDirectSharedOwnerEntry() {
 s.mu.Lock();defer s.mu.Unlock()
 if s.patchLaps==nil || s.patchLaps.sharedIntervalLimit<=0 {return}
 if s.patchLaps.sharedDirectEntry {s.patchLaps.sharedIntervalLimit=-1;s.patchLaps.sharedIntervals=nil;return}
 s.patchLaps.sharedDirectEntry=true
}

// Only the actual branch owner may supply these fixed source dispositions.
// Point collectors do not allocate a map or acquire new timing readings.
func (s *PublicationStamps) markSharedOptionalDisposition(name, reason string) {
 if s==nil {return};s.mu.Lock();defer s.mu.Unlock()
 p:=s.patchLaps;if p==nil||p.sharedIntervalLimit<=0||!p.sharedDirectEntry{return}
 if p.sharedOptionalDispositions==nil {p.sharedOptionalDispositions=make(map[string]string,2)}
 if old:=p.sharedOptionalDispositions[name];old!=""&&old!=reason {p.sharedIntervalLimit=-1;p.sharedIntervals=nil;return}
 p.sharedOptionalDispositions[name]=reason
}
