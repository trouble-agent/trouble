# README-8 gate arm evidence — trouble-foreman-2026-11-26-11-51-00
date: 2026-11-26T11:54:29Z head: 1b413aa9cc33594bbb3d5563db147b25fd0fdc88

$ trouble --version (freshly reinstalled from HEAD)
0.0.0-dev unknown 1970-01-01T00:00:00.000Z [UNSTAMPED (degraded; trouble install refuses to enable the unit without --force)]

$ troubled --help | grep state
  --state-root spelling is retained as a compatibility alias, and both forms
  are accepted on every config-resolving surface.


$ trouble config explain --state-root /home/kara/.hermes/stand-in/readme/trouble/state-alias --json
exit=0
{"key":"state_root","value":"/home/kara/.hermes/stand-in/readme/trouble/state-alias","source":"flag","source_ref":"--state_root","redacted":false}
