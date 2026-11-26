# Include from your Makefile:  include path/to/host-watcher/examples/gate-in-makefile.mk
# Then heavy targets run through the gate:  $(GATE) go test ./...
HOST_WATCHER_DIR ?= $(dir $(lastword $(MAKEFILE_LIST)))..
PYTHON ?= python3
GATE = $(PYTHON) $(HOST_WATCHER_DIR)/scripts/host_gate.py --max-wait 900 --
