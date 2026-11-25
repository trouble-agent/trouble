Audit trouble-readme-2026-11-25-22-31-35; published source d6b8ebf679a9547ce1095516d5ae0cada52401d1
README.md:255-256 links to deploy/README.md:20-23, which says --state-root is refused as an unknown flag.
Stranger install: go install github.com/trouble-agent/trouble/cmd/trouble@latest and go install github.com/trouble-agent/trouble/cmd/troubled@latest each exited 0, using a fresh HOME and a public-origin checkout at the published source above.
Command: /home/kara/.hermes/stand-in/readme/trouble/run-20261125-223135/home/go/bin/troubled --help
Observed: "The documented --state-root spelling is retained as a compatibility alias, and both forms are accepted on every config-resolving surface."
Command: /home/kara/.hermes/stand-in/readme/trouble/run-20261125-223135/home/go/bin/trouble config explain --state-root /home/kara/.hermes/stand-in/readme/trouble/state-alias --json
Exit 0; the state_root entry returned value /home/kara/.hermes/stand-in/readme/trouble/state-alias, source flag, source_ref --state_root, redacted false.
The executable accepts and applies the dash alias; the linked quickstart's refusal claim is stale. A disposable config-explain run is not a daemon boot test.
