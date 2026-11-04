package main

import (
 "bytes"
 "encoding/json"
 "fmt"
 "os"
 "path/filepath"
 "strings"
 "testing"
)

// Synthetic arithmetic/contract inputs, never claimed runtime evidence.
func TestReadOnlySeedCheckerRequiresMeasuredDoubling(t *testing.T) {
 for _, tc := range []struct{name string; on float64; want int; doubled bool}{
  {"flag_without_slowdown",1100,0,false},
  {"below_double",2199,0,false},
  {"exact_double",2200,1,true},
  {"measured_double",3300,1,true},
 } {
  t.Run(tc.name,func(t *testing.T){
   off,on := readonlySeedInputFiles(t,1100,1100,tc.on)
   var out, diagnostics bytes.Buffer
   code := run([]string{"seed-search","--off-inputs",off,"--on-inputs",on,"--host-class","reference_linux"}, &out,&diagnostics)
   if code != tc.want || !strings.Contains(out.String(),"search_symbols_p95") || !strings.Contains(out.String(),fmt.Sprintf("measured_doubling=%t",tc.doubled)) { t.Fatalf("unmeasured/incorrect gate: exit%d out=%s err=%s",code,&out,&diagnostics) }
  })
 }
}

func readonlySeedInputFiles(t *testing.T, mainMS, offMS, onMS float64) (string,string) {
 t.Helper()
 dir := t.TempDir()
 makeInputs := func(seeded bool) string {
  c := inputs{Version:1,CandidateRevision:strings.Repeat("a",40),MainRevision:strings.Repeat("d",40),TagRevision:shippedRevision,Repeats:3,Hosts:map[string]string{"reference_linux":"synthetic-host"}}
  group := "off"
  if seeded { group="on"; c.NightlySeed=&nightlyWorkloadSeed{Version:1,Kind:"search-delay-read16-v1",CandidateRevision:c.CandidateRevision} }
  roles := []string{"main","candidate"}; if seeded { roles=[]string{"candidate"} }
  for repeat:=1;repeat<=3;repeat++ { for _,role:=range roles {
   raw:=syntheticReport(); raw["run_id"]=fmt.Sprintf("%s-%s-%d",group,role,repeat)
   if role=="main" {raw["git_sha"]=c.MainRevision}
   raw["workload"].(map[string]any)["agents"]=16
   p:=raw["phases"].([]any)[0].(map[string]any)
   ms:=offMS; if role=="main" {ms=mainMS}; if seeded {ms=onMS}
   p["tools"].(map[string]any)["search.symbols"].(map[string]any)["latency_ms"]=map[string]any{"count":1,"p95":map[string]any{"value":ms,"unit":"ms","reason":""}}
   p["attempts"].([]any)[0].(map[string]any)["logical_ms"]=ms
   if seeded {raw["nightly_workload_seed"]="search-delay-read16-v1"; a:=p["attempts"].([]any)[0].(map[string]any); a["nightly_workload_seed"]="search-delay-read16-v1"; a["nightly_delay_ns"]=1000000000}
   path:=filepath.Join(dir,fmt.Sprintf("%s-%s-%d.json",group,role,repeat)); data,err:=json.Marshal(raw); if err!=nil {t.Fatal(err)}; if err:=os.WriteFile(path,data,0600);err!=nil {t.Fatal(err)}
   c.Runs=append(c.Runs,runInput{Role:role,HostClass:"reference_linux",Scenario:"load-16",Repeat:repeat,Report:path})
  }}
  path:=filepath.Join(dir,group+"-inputs.json"); data,err:=json.Marshal(c);if err!=nil {t.Fatal(err)};if err:=os.WriteFile(path,data,0600);err!=nil {t.Fatal(err)};return path
 }
 return makeInputs(false),makeInputs(true)
}

func TestReadOnlySeedRoutesWithoutShrinkingExistingMatrix(t *testing.T) {
 data,err:=os.ReadFile("../budgets.yaml");if err!=nil{t.Fatal(err)}
 p,err:=loadPolicy(data);if err!=nil{t.Fatal(err)}
 c:=fixtureLaneConfig();c.Lane="nightly";c.HostClass="reference_linux"
 plain,err:=makeLanePlan(p,c);if err!=nil{t.Fatal(err)}
 c.NightlySeed=&nightlyWorkloadSeed{Version:1,Kind:"search-delay-read16-v1",CandidateRevision:c.Builds["candidate"].Revision}
 seeded,err:=makeLanePlan(p,c);if err!=nil{t.Fatal(err)}
 if len(plain.Tasks)!=len(seeded.Tasks) || plain.PolicySHA256!=seeded.PolicySHA256 {t.Fatal("read-only seed changed full matrix or release policy")}
 for i,task:=range seeded.Tasks {
  a,_:=json.Marshal(plain.Tasks[i]);b,_:=json.Marshal(task);if !bytes.Equal(a,b) {t.Fatal("seed changed workload/task identity")}
  args:=strings.Join(taskArgs(c,task,"manifest","scratch","report")," ")
  if strings.Contains(args,"--nightly-workload-seed")!=(task.Role=="candidate" && task.Scenario=="load-16") {t.Fatalf("seed escaped read16: %+v",task)}
 }
}

func TestReadOnlySeedCheckerRejectsMissingOrIncomparableEvidence(t *testing.T) {
 for _,kind:=range []string{"missing_repeat","wrong_scenario","no_seed_echo","changed_deadline","reused_identity"} {t.Run(kind,func(t *testing.T){
  off,on:=readonlySeedInputFiles(t,1100,1100,2200)
  data,err:=os.ReadFile(on);if err!=nil{t.Fatal(err)}
  var c inputs;if err:=json.Unmarshal(data,&c);err!=nil{t.Fatal(err)}
  switch kind {
  case "missing_repeat":c.Runs=c.Runs[:2]
  case "wrong_scenario":c.Runs[0].Scenario="load-8"
  default:
   path:=c.Runs[0].Report;data,err:=os.ReadFile(path);if err!=nil{t.Fatal(err)}
   var r map[string]any;if err:=json.Unmarshal(data,&r);err!=nil{t.Fatal(err)}
   switch kind {case "no_seed_echo":delete(r,"nightly_workload_seed");case "changed_deadline":r["workload"].(map[string]any)["product_deadline_seconds"]=121;case "reused_identity":r["run_id"]="off-candidate-1"}
   data,err=json.Marshal(r);if err!=nil{t.Fatal(err)};if err:=os.WriteFile(path,data,0600);err!=nil{t.Fatal(err)}
  }
  data,err=json.Marshal(c);if err!=nil{t.Fatal(err)};if err:=os.WriteFile(on,data,0600);err!=nil{t.Fatal(err)}
  var out,diagnostics bytes.Buffer
  if code:=run([]string{"seed-search","--off-inputs",off,"--on-inputs",on,"--host-class","reference_linux"},&out,&diagnostics);code!=2 {t.Fatalf("unproved evidence became numerical breach/pass: %d %s %s",code,&out,&diagnostics)}
 })}
}
