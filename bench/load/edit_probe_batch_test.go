package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func editProbeBatchACKAbsPath()string{return filepath.Join(os.TempDir(),"edit-probe-owned-repo","file.go")}
func editProbeBatchOfferedBytes()[]byte{return []byte("package fixture\nfunc Added() { Callee() }\n")}
func editProbeBatchOfferedSHA()string{hash:=sha256.Sum256(editProbeBatchOfferedBytes());return hex.EncodeToString(hash[:])}

func editProbeBatchACKFixture()map[string]any{
	return map[string]any{"transaction_id":"transaction-1","status":"committed","disk_status":"committed","acknowledgement_receipt":"batch-ack-2","acknowledgement_state":"reference_only_query_mutation_status","files":[]any{map[string]any{"path":editProbeBatchACKAbsPath(),"relative_path":"repo/file.go","after_sha256":editProbeBatchOfferedSHA(),"reindex_receipt":"mutation-3","reindex_generation":json.Number("4")}},"results":[]any{map[string]any{"op":"edit_file","path":"repo/file.go","status":"applied","reindex_receipt":"mutation-3","reindex_generation":json.Number("4")}}}
}

func TestEditProbeBatchKeepsActualTransactionACKAndFileIdentities(t *testing.T){
	refs,err:=decodeEditProbeBatchAcknowledgement(editProbeBatchACKFixture(),"transaction-1",editProbeBatchACKAbsPath(),"repo/file.go",editProbeBatchOfferedBytes())
	if err!=nil || refs.validateReferences()!=nil {t.Fatal(refs,err)}
	if refs.TransactionID!="transaction-1" || refs.Acknowledgement!="batch-ack-2" || refs.GraphReceipt!="mutation-3" || refs.GraphGeneration!=4 || refs.AfterSHA256!=editProbeBatchOfferedSHA() {t.Fatal(refs)}
}

func TestEditProbeBatchRejectsWrongSourceVerdictOrFileJoin(t *testing.T){
	for _,name:=range []string{"transaction","rollback","pending","files","results","wrong_path","wrong_relative","wrong_result_path","wrong_op","failed_result","absent","malformed_absent","hash","foreign_after_hash","generation"} {t.Run(name,func(t *testing.T){
		body:=editProbeBatchACKFixture();file:=body["files"].([]any)[0].(map[string]any);result:=body["results"].([]any)[0].(map[string]any)
		switch name {case "transaction":body["transaction_id"]="foreign";case "rollback":body["disk_status"]="rolled_back";case "pending":body["status"]="prepared";case "files":body["files"]=[]any{file,file};case "results":body["results"]=[]any{};case "wrong_path":file["path"]="/foreign/file.go";case "wrong_relative":file["relative_path"]="foreign/file.go";case "wrong_result_path":result["path"]="foreign/file.go";case "wrong_op":result["op"]="edit_symbol";case "failed_result":result["status"]="failed";case "absent":file["after_absent"]=true;case "malformed_absent":file["after_absent"]="false";case "hash":file["after_sha256"]=strings.Repeat("b",40);case "foreign_after_hash":file["after_sha256"]=strings.Repeat("b",64);case "generation":file["reindex_generation"]=json.Number("1.5")}
		if _,err:=decodeEditProbeBatchAcknowledgement(body,"transaction-1",editProbeBatchACKAbsPath(),"repo/file.go",editProbeBatchOfferedBytes());err==nil {t.Fatal("accepted contradictory durable ACK",name)}
	})}
}

func TestEditProbeBatchMissingReferencesDoNotEraseCommittedACK(t *testing.T){
	for _,name:=range []string{"missing_A","missing_graph","wrong_result_graph","wrong_result_generation","swapped_domains"} {t.Run(name,func(t *testing.T){
		body:=editProbeBatchACKFixture();file:=body["files"].([]any)[0].(map[string]any);result:=body["results"].([]any)[0].(map[string]any)
		switch name {case "missing_A":delete(body,"acknowledgement_receipt");case "missing_graph":delete(file,"reindex_receipt");delete(file,"reindex_generation");case "wrong_result_graph":result["reindex_receipt"]="foreign";case "wrong_result_generation":result["reindex_generation"]=json.Number("5");case "swapped_domains":file["reindex_receipt"]="batch-ack-2";result["reindex_receipt"]="batch-ack-2"}
		refs,err:=decodeEditProbeBatchAcknowledgement(body,"transaction-1",editProbeBatchACKAbsPath(),"repo/file.go",editProbeBatchOfferedBytes());if err!=nil {t.Fatal("lost genuine committed source ACK",err)};if refs.validateReferences()==nil {t.Fatal("accepted unavailable or foreign accounting",refs)}
	})}
}

func TestEditProbeBatchRequestUsesPublicChangesAndTransactionOption(t *testing.T){
	c:=Corpus{File:"file.go",EditAnchor:"// anchor\n",EditTemplate:"func {{marker}}() { Callee() }\n"}
	before:="package fixture\n// anchor\n"
	r,after:=editProbeBatchRequest(c,before,"Marker","transaction-1")
	if r.name()!="edit.batch" {t.Fatal(r)}
	expectedAfter:="package fixture\n// anchor\nfunc Marker() { Callee() }\n"
	expected:=map[string]any{"operation":"batch","changes":[]any{map[string]any{"op":"edit_file","path":"file.go","old_string":before,"new_string":expectedAfter}},"options":map[string]any{"transaction_id":"transaction-1"}}
	if after!=expectedAfter {t.Fatal("changed offered intent",after)}
	if !reflect.DeepEqual(r.Arguments,expected) {t.Fatal("wrong public batch wire shape",r.Arguments)}
	if _,present:=r.Arguments["mutation_id"];present {t.Fatal("substituted single-edit identity")}
}
