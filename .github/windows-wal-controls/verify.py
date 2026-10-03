import sys,re
from pathlib import Path
mode,path=sys.argv[1:];s=Path(path).read_text(encoding="utf-8",errors="replace")
assert "COPY_GUARD_CONTROL kind=cancelled forced_main_sync=true minimum_requested=6s" in s,s[-6000:]
if mode=="old":
    assert "--- FAIL: TestSlowConvergenceProofRefusesInvalidCopies" in s,s[-6000:]
    assert 'Error:       "0" is not positive' in s or '"0" is not positive' in s,s[-6000:]
    assert "first_scan_error=context deadline exceeded" in s,s[-6000:]
    assert "actual_first_tuple={Busy:0 WALFrames:0 CheckpointedFrames:0}" in s,s[-6000:]
elif mode=="new":
    assert "--- FAIL:" not in s,s[-6000:]
    assert "\nFAIL" not in s,s[-6000:]
    assert "--- PASS: TestSlowConvergenceProofRefusesInvalidCopies" in s,s[-6000:]
    assert "copy_setup_budget=30s controlled_cancel_requested=true" in s,s[-6000:]
    assert "actual_first_tuple={Busy:0 WALFrames:501 CheckpointedFrames:501}" in s,s[-6000:]
    assert "first_scan_error=<nil>" in s,s[-6000:]
    assert "context=context canceled" in s,s[-6000:]
else:raise ValueError(mode)
print("QUALIFIED_"+mode.upper()+"_COMPLETED_COPY_BOUNDARY")
