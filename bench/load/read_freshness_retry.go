package main

const readFreshnessRetryLimit = 2

func readFreshnessCode(code string) bool {
 return code=="projection_superseded" || code=="file_view_pending"
}

func readFreshnessRequest(r request,args map[string]any,role string) bool {
 if role!="workload" || r.Tool!="read" || args["operation"]!=r.Operation || r.Operation!="summary" && r.Operation!="editing_context" {return false}
 if r.Operation=="summary" {return true}
 values:=[]map[string]any{args}
 if value,present:=args["options"];present {options,valid:=value.(map[string]any);if !valid {return false};values=append(values,options)}
 for _,options:=range values {if value,present:=options["compress_bodies"];present {compressed,valid:=value.(bool);if !valid || compressed {return false}}}
 return true
}

func (e *rpcError) retryReadFreshness(r request,args map[string]any,role string) bool {
 if e==nil || e.Code!=-32000 || !readFreshnessRequest(r,args,role) {return false}
 code,_:=e.Data["error_code"].(string);retriable,_:=e.Data["retriable"].(bool)
 unknown,known:=e.Data["delivery_unknown"].(bool)
 return readFreshnessCode(code) && retriable && known && !unknown && e.Data["retry_scope"]=="read_only"
}
