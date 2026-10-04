package indexer

import (
 "encoding/json"
 "fmt"
 "sort"

 "github.com/zzet/gortex/internal/graph"
)

const contractCoreInputVersion = "contract-core-input-v1"

// The positive input journal is cumulative from its selected top-positive
// predecessor, never from inherited live0. Live0 is composed separately by
// contract consumers/workers. Source-only changes carry this identity exactly.
func nextContractCoreInputState(previous *graph.ContractInputState,repo,checkout string,changes []contractCoreInputChange)(graph.ContractInputState,bool,error) {
 next:=graph.ContractInputState{RepoPrefix:repo,CheckoutID:checkout,InputVersion:contractCoreInputVersion}
 if previous!=nil {
  if previous.RepoPrefix!=repo || previous.InputVersion=="" || previous.InputFingerprint=="" {return next,false,fmt.Errorf("contract core inputs: invalid selected predecessor")}
  next=*previous;next.RepoPrefix=repo;next.CheckoutID=checkout
 }
 type inputChange struct {
  File string
  Deleted bool
  Unknown string
  Current *contractBoundaryReceipt
  Keys []string
 }
 var relevant []inputChange
 for _,change:=range changes {
  if len(change.Delta.Scope.Causes)==0 && !change.Delta.Scope.Unknown && len(change.Dependents)==0 {continue}
  var current *contractBoundaryReceipt
  if change.Current!=nil {
   detached:=*change.Current
   // Actual accepted bytes remain source eligibility evidence in the durable
   // receipt. They are not a contract identity or every body edit would move it.
   detached.Source=""
   current=&detached
  }
  relevant=append(relevant,inputChange{File:change.FilePath,Deleted:change.Deleted,Unknown:change.Uncertainty,Current:current,Keys:appendUniqueSorted(nil,change.Delta.ChangedProducedKeys...)})
 }
 if len(relevant)==0 && previous!=nil {return next,false,nil}
 sort.Slice(relevant,func(i,j int)bool{return relevant[i].File<relevant[j].File})
 encoded,err:=json.Marshal(struct {
  Version string
  PreviousVersion,PreviousFingerprint string
  MissingBaseline bool
  Changes []inputChange
 }{contractCoreInputVersion,next.InputVersion,next.InputFingerprint,previous==nil,relevant})
 if err!=nil {return next,false,err}
 next.InputVersion=contractCoreInputVersion
 next.InputFingerprint=contractInputHash(encoded)
 next.Accepted=false
 return next,true,nil
}

// Work belongs only to the source actor whose contract inputs advanced.
// Other receipt owners are scheduling/dependency-vector inputs, not mutable
// targets. The full immutable scope is retained in every token.
func contractCoreWorkForChanges(generation int64,state graph.ContractInputState,changes []contractCoreInputChange)([]graph.ContractWork,error) {
 var work []graph.ContractWork
 for _,change:=range changes {
  scope:=change.Delta.Scope
  if len(scope.Causes)==0 && !scope.Unknown {
   if len(change.Dependents)==0 {continue}
   scope.Causes=[]string{"dependency_input_changed"}
  }
  if len(change.Dependents)>0 {scope.LookupKeys=appendUniqueSorted(scope.LookupKeys,change.Delta.ChangedProducedKeys...)}
  scope.Deleted=change.Deleted
  row:=graph.ContractWork{OriginGeneration:generation,CheckoutID:state.CheckoutID,RepoPrefix:state.RepoPrefix,FilePath:change.FilePath,InputVersion:state.InputVersion,InputFingerprint:state.InputFingerprint,State:graph.ContractWorkPending,Scope:scope}
  payload,err:=json.Marshal(row)
  if err!=nil {return nil,err}
  row.Token=contractInputHash(payload)
  work=append(work,row)
 }
 sort.Slice(work,func(i,j int)bool{return work[i].Token<work[j].Token})
 return work,nil
}
