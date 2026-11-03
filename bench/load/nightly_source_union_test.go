package main

import "testing"

func TestNightlyUnionPreservesStrictSearchACKAndSmokeOptions(t *testing.T) {
 // Current B's search acceptance profile still selects local ACK and rejects
 // explicit full ACK after the nightly options and smoke v2 fields are added.
 args:=append(searchAcceptanceArgs(4,1000),"--search-readback-acceptance","--edit-mean=1s")
 options,err:=parseRun(args)
 if err!=nil||!options.SearchReadbackAcceptance||options.editACKPolicy()!="local"||options.NightlySeed!="" {t.Fatalf("strict search union lost: %+v %v",options,err)}
 if _,err:=parseRun(append(args,"--edit-ack=full"));err==nil {t.Fatal("source union weakened B's full-ACK refusal")}
 read:=[]string{"--binary","unused","--manifest","unused","--scratch",t.TempDir(),"--scenario","read","--agents","8","--edit-agents","0","--worktrees","0","--duration","20s","--deadline","120s","--nightly-workload-seed","search-delay-v1"}
 options,err=parseRun(read)
 if err!=nil||options.NightlySeed!="search-delay-v1"||options.PRSmokeDrain!=0 {t.Fatalf("nightly union lost: %+v %v",options,err)}
 if _,err:=parseRun(append(read,"--pr-smoke-drain=2m","--pr-smoke-drain-version=1"));err==nil {t.Fatal("nightly fault accepted smoke-only offered/drain envelope")}
}
