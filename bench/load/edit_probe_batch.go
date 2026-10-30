package main

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
)

// Batch source authority is the durable transaction. The acknowledgement RPC
// and this file's graph ticket have separate clocks/retention and are never
// represented as one synthetic mutation_receipt.
type editProbeBatchReferences struct {
	TransactionID string `json:"transaction_id"`
	Acknowledgement string `json:"acknowledgement_receipt"`
	Path string `json:"path"`
	RelativePath string `json:"relative_path"`
	AfterSHA256 string `json:"after_sha256"`
	GraphReceipt string `json:"reindex_receipt"`
	GraphGeneration uint64 `json:"reindex_generation"`
	ResultReceipt string `json:"result_reindex_receipt"`
	ResultGeneration uint64 `json:"result_reindex_generation"`
	AcknowledgementState string `json:"acknowledgement_state"`
	TransactionError string `json:"transaction_error,omitempty"`
}

func editProbeBatchRequest(c Corpus, marker, transactionID string) request {
	return request{Tool:"edit",Operation:"batch",Arguments:map[string]any{
		"operation":"batch",
		"changes":[]any{map[string]any{"op":"edit_file","path":c.File,"old_string":c.EditAnchor,"new_string":c.EditAnchor+strings.ReplaceAll(c.EditTemplate,"{{marker}}",marker)}},
		"options":map[string]any{"transaction_id":transactionID},
	}}
}

// Only the pointed one-file edit_file shape is supported here. Missing timing
// references do not negate a genuine committed ACK: validateReferences below
// reports those independently, so unavailable accounting cannot become zero or
// remove the successful source acknowledgement from its denominator.
func decodeEditProbeBatchAcknowledgement(body map[string]any,transactionID,absPath,relativePath string)(editProbeBatchReferences,error){
	out:=editProbeBatchReferences{}
	if transactionID=="" || body["transaction_id"]!=transactionID || body["status"]!="committed" || body["disk_status"]!="committed" {return out,errors.New("batch transaction is not the requested committed source acknowledgement")}
	files,ok:=body["files"].([]any);if !ok || len(files)!=1 {return out,errors.New("one-file batch acknowledgement requires exactly one durable file")}
	results,ok:=body["results"].([]any);if !ok || len(results)!=1 {return out,errors.New("one-file batch acknowledgement requires exactly one applied result")}
	file,ok:=files[0].(map[string]any);if !ok {return out,errors.New("invalid durable file")}
	result,ok:=results[0].(map[string]any);if !ok || result["op"]!="edit_file" || result["status"]!="applied" || result["path"]!=relativePath {return out,errors.New("batch result does not identify the applied requested edit_file")}
	path,_:=file["path"].(string);rel,_:=file["relative_path"].(string)
	if !filepath.IsAbs(absPath) || filepath.Clean(absPath)!=absPath || path!=absPath || rel!=relativePath || relativePath=="" || file["after_absent"]==true || file["before_absent"]==true {return out,errors.New("batch durable file identity does not match the existing requested file")}
	for _,name:=range []string{"after_absent","before_absent"} {if value,present:=file[name];present {if absent,ok:=value.(bool);!ok || absent {return out,errors.New("batch file absence flag contradicts existing-file edit")}}}
	after,_:=file["after_sha256"].(string);bytes,err:=hex.DecodeString(after)
	if err!=nil || len(bytes)!=32 || after!=strings.ToLower(after) {return out,errors.New("batch durable after-image SHA256 unavailable")}
	out.TransactionID,out.Path,out.RelativePath,out.AfterSHA256=transactionID,path,rel,after
	if value,present:=body["error"];present {message,ok:=value.(string);if !ok {return out,errors.New("invalid transaction error")};out.TransactionError=message}
	out.Acknowledgement,_=body["acknowledgement_receipt"].(string)
	out.AcknowledgementState,_=body["acknowledgement_state"].(string)
	out.GraphReceipt,_=file["reindex_receipt"].(string)
	if value,present:=file["reindex_generation"];present {generation,err:=strconvEditProbeUint(value);if err!=nil {return out,errors.New("invalid batch file generation")};out.GraphGeneration=generation}
	out.ResultReceipt,_=result["reindex_receipt"].(string)
	if value,present:=result["reindex_generation"];present {generation,err:=strconvEditProbeUint(value);if err!=nil {return out,errors.New("invalid batch result generation")};out.ResultGeneration=generation}
	return out,nil
}

func (r editProbeBatchReferences) validateReferences()error{
	if !strings.HasPrefix(r.Acknowledgement,"batch-ack-") || r.Acknowledgement=="batch-ack-" || r.AcknowledgementState!="reference_only_query_mutation_status" {return errors.New("original shared batch acknowledgement reference unavailable")}
	if r.GraphReceipt=="" || r.GraphGeneration==0 {return errors.New("original batch file graph reference unavailable")}
	if r.ResultReceipt!=r.GraphReceipt || r.ResultGeneration!=r.GraphGeneration {return errors.New("batch result and durable file graph identities disagree")}
	if r.TransactionID==r.Acknowledgement || r.TransactionID==r.GraphReceipt || r.Acknowledgement==r.GraphReceipt {return errors.New("batch transaction/acknowledgement/file identities are not distinct")}
	return nil
}
