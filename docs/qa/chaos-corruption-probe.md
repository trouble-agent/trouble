# Fleet QA corruption/restart probe

The executable `.qa/chaos-probes/corruption.sh` replaces the fleet harness's generic corruption detector for the `chaos-corruption` cell. This matters for trouble because its state root is outside the repository by default, so the generic repo scan cannot identify the daemon's real state and would report N/A.

The probe builds `bin/troubled` when it is missing, creates a temporary state root and minimal config, and starts the daemon against that isolated state. After confirming the initial heartbeat, it stops the daemon, truncates only the temporary `heartbeat.json` to 64 bytes, and restarts with the same config. A rewritten heartbeat is recovery; an immediate non-zero refusal is also an acceptable clean outcome. A successful exit without rewriting the heartbeat, or a restart still running after 30 seconds with no rewrite, is treated as misbehavior. The probe kills its daemon process group and removes the temporary directory on every exit path.

Exit codes follow the harness contract:

- `0`: corruption restart recovered or was cleanly refused; the first stdout line is the verdict.
- `1`: observed application misbehavior (silent successful exit without recovery, or a hung restart).
- `2`: environment gap; the probe could not build or establish the initial test run.
- `124`: reserved by the harness timeout for a probe exceeding 120 seconds.

The harness runs this probe with a 120-second outer timeout and grades these codes as OK, FAIL, INFO, and FAIL respectively. The probe's own boot and restart waits are deadline-bounded; its expected runtime is well below the harness limit. No live Trouble state is read or modified.
