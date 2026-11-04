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
