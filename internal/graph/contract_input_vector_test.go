package graph

import "testing"

func TestContractInputVectorStableCarryPendingAndPredecessor(t *testing.T){
 primary:=ContractInputState{RepoPrefix:"repo",InputVersion:"v1",InputFingerprint:"a",Accepted:true}
 carried:=primary;carried.CheckoutID="wt"
 witnesses:=[]ContractInputWitness{{GenerationID:0,State:primary,Found:true},{GenerationID:4,State:carried,Found:true}}
 single,err:=ComposeContractInputState("repo","wt",witnesses);if err!=nil||single.InputFingerprint!="a"||!single.Accepted{t.Fatalf("carry=%#v %v",single,err)}
 changed:=carried;changed.InputFingerprint="b";changed.PreviousInputVersion="v1";changed.PreviousInputFingerprint="a"
 witnesses[1].State=changed
 combined,err:=ComposeContractInputState("repo","wt",witnesses);if err!=nil||combined.InputVersion!="contract-selected-input-v1"||combined.InputFingerprint=="a"{t.Fatalf("combined=%#v %v",combined,err)}
 // Superseded positive summaries are not logical components. Top cumulative
 // authority survives a fold removing distinct intermediate positive states.
 older:=carried;older.InputFingerprint="distinct-older"
 expanded,err:=ComposeContractInputState("repo","wt",[]ContractInputWitness{witnesses[0],{GenerationID:2,State:older,Found:true},witnesses[1]});if err!=nil||expanded!=combined{t.Fatal("historical positive leaked into logical key")}
 witnesses[1].GenerationID=100
 folded,err:=ComposeContractInputState("repo","wt",witnesses);if err!=nil||folded!=combined{t.Fatal("fold changed logical identity")}
 witnesses[1].State.Accepted=false
 pending,err:=ComposeContractInputState("repo","wt",witnesses);if err!=nil||pending.Accepted{t.Fatal("pending fell through accepted lower")}
 previous,err:=ComposePreviousContractInputState("repo","wt",witnesses);if err!=nil||previous.InputFingerprint!="a"{t.Fatalf("predecessor=%#v %v",previous,err)}
 updatedBase:=primary;updatedBase.InputFingerprint="primary-new"
 changedSparse,err:=ComposeContractInputState("repo","wt",[]ContractInputWitness{{GenerationID:0,State:updatedBase,Found:true},{GenerationID:100,State:changed,Found:true}});if err!=nil||changedSparse.InputFingerprint==combined.InputFingerprint{t.Fatal("inherited0 change ignored")}
 dedicated,err:=ComposeContractInputState("repo","wt",[]ContractInputWitness{{GenerationID:100,State:changed,Found:true}});if err!=nil||dedicated.InputFingerprint!="b"{t.Fatal("full root did not omit0")}
 if _,err:=ComposeContractInputState("repo","wt",[]ContractInputWitness{{GenerationID:0,State:primary,Found:false},{GenerationID:100,State:changed,Found:true}});err==nil{t.Fatal("missing inherited baseline certified by positive")}
 foreign:=primary;foreign.RepoPrefix="foreign"
 if _,err:=ComposeContractInputState("repo","wt",[]ContractInputWitness{{GenerationID:1,State:foreign,Found:true}});err==nil{t.Fatal("foreign only vector certified root repo")}
 if _,err:=ComposeContractInputState("repo","wt",[]ContractInputWitness{{Found:false}});err==nil{t.Fatal("empty vector became usable")}
}
