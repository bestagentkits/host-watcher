## Host pressure

Several agents may share this machine. Before fanning out parallel work, run
`python <host-watcher>/scripts/host_status.py`. Run heavy builds, tests,
installs and preflights through
`python <host-watcher>/scripts/host_gate.py -- <cmd>`: it waits while the
machine is saturated, then runs the command unchanged and returns its exit
code. If `python <host-watcher>/scripts/host_inbox.py --take` prints a notice,
follow it. This paces commands; it never cuts task scope.
