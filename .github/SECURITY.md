# Security policy

host-watcher runs locally, reads OS counters and the process table, and
deletes files only through `cache_cleanup.py --apply`. Please report
vulnerabilities, especially any way to make cleanup remove data outside the
documented rules, through GitHub private vulnerability reporting on this
repository rather than a public issue.
