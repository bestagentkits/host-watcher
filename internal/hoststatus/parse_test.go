package hoststatus

import (
	"math"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestParseProcStatAndBusy(t *testing.T) {
	prev, err := parseProcStat("cpu  100 0 100 700 100 0 0 0 50 0\ncpu0 1 2 3 4 5\nintr 1\n")
	if err != nil {
		t.Fatal(err)
	}
	// Guest (50) is excluded; idle = idle+iowait.
	if prev.total != 1000 || prev.idle != 800 {
		t.Fatalf("prev = %+v", prev)
	}
	cur, err := parseProcStat("cpu  200 0 200 750 150 0 0 0 0 0\n")
	if err != nil {
		t.Fatal(err)
	}
	busy, ok := busyPct(prev, cur)
	if !ok || !near(busy, 66.66666666666667) {
		t.Fatalf("busy = %v ok=%v", busy, ok)
	}
	if _, ok := busyPct(cur, prev); ok {
		t.Fatal("counters that did not advance must not yield a sample")
	}
	for _, bad := range []string{"", "intr 1\n", "cpu x 1 2 3 4\n"} {
		if _, err := parseProcStat(bad); err == nil {
			t.Fatalf("parseProcStat(%q) want error", bad)
		}
	}
}

func TestParseMeminfo(t *testing.T) {
	m, err := parseMeminfo("MemTotal:       16777216 kB\nMemFree:  1 kB\nMemAvailable:    4194304 kB\n")
	if err != nil {
		t.Fatal(err)
	}
	if m.TotalGB != 16 || m.AvailableGB != 4 || m.UsedPct != 75 {
		t.Fatalf("memory = %+v", m)
	}
	if _, err := parseMeminfo("MemTotal: 10 kB\n"); err == nil {
		t.Fatal("missing MemAvailable must error")
	}
}

const diskstatsA = `   7       0 loop0 10 0 10 10 0 0 0 0 0 10 10 0 0 0 0
   8       0 sda 1000 0 2000 500 2000 0 4000 1500 0 10000 2000 0 0 0 0
   8       1 sda1 900 0 1800 450 1900 0 3800 1400 0 9000 1800 0 0 0 0
 259       0 nvme0n1 10 0 20 5 10 0 20 5 0 100 10
  11       0 sr0 1 0 1 1 0 0 0 0 0 1 1
`

const diskstatsB = `   7       0 loop0 99 0 99 99 0 0 0 0 0 999 99 0 0 0 0
   8       0 sda 1100 0 4048 700 2100 0 8096 1900 0 10900 2600 0 0 0 0
   8       1 sda1 990 0 3800 650 1990 0 7800 1800 0 9900 2400 0 0 0 0
 259       0 nvme0n1 10 0 20 5 10 0 20 5 0 100 10
  11       0 sr0 1 0 1 1 0 0 0 0 0 1 1
`

func wholeDisks(name string) bool { return name == "sda" || name == "nvme0n1" || name == "sr0" }

func TestParseDiskstatsAndRates(t *testing.T) {
	a, err := parseDiskstats(diskstatsA, wholeDisks)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 2 || a["sda"].ioTicks != 10000 {
		t.Fatalf("whole disks = %+v (loop, sr and partitions must be excluded)", a)
	}
	b, err := parseDiskstats(diskstatsB, wholeDisks)
	if err != nil {
		t.Fatal(err)
	}
	rates := diskstatsRates(a, b, 1000)
	sda := rates["sda"]
	// io_ticks +900 over 1000 ms; (200+400) ms wait over 200 ios; sectors*512.
	if !near(*sda.active, 90) || !near(*sda.latencyMs, 3) ||
		!near(*sda.writeMBs, 4096*512/1e6) || !near(*sda.readMBs, 2048*512/1e6) {
		t.Fatalf("sda = active %v latency %v write %v read %v", *sda.active, *sda.latencyMs, *sda.writeMBs, *sda.readMBs)
	}
	if idle := rates["nvme0n1"]; *idle.active != 0 || *idle.latencyMs != 0 {
		t.Fatalf("idle disk = %+v", idle)
	}
	if back := diskstatsRates(b, a, 1000); len(back) != 1 || back["sda"].active != nil {
		t.Fatalf("sda went backwards and must be skipped (unchanged nvme stays), got %v", back)
	}
	if _, err := parseDiskstats("8 0 sda x 0 0 0 0 0 0 0 0 0 0\n", wholeDisks); err == nil {
		t.Fatal("non-numeric field must error")
	}
}

const vmStatFixture = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                3000.
Pages active:                            200000.
Pages inactive:                          100000.
Pages speculative:                         1000.
Pages throttled:                              0.
Pages wired down:                         90000.
`

func TestParseVMStat(t *testing.T) {
	avail, err := parseVMStat(vmStatFixture)
	if err != nil {
		t.Fatal(err)
	}
	if avail != 104000*16384 {
		t.Fatalf("available = %d", avail)
	}
	if _, err := parseVMStat("Pages free: 1.\n"); err == nil {
		t.Fatal("missing page size must error")
	}
	if _, err := parseVMStat("(page size of 4096 bytes)\nPages active: 1.\n"); err == nil {
		t.Fatal("missing free/inactive counts must error")
	}
}

const iostatFixture = `              disk0               disk4       cpu
    KB/t  tps  MB/s     KB/t  tps  MB/s  us sy id
   25.10   40  0.98    12.00    1  0.01   7  4 89
   30.00  100  2.93    10.00    5  0.05  20 10 70
   20.00   10  0.20    50.00   80  3.91  50 30 20
`

func TestParseIostat(t *testing.T) {
	rows, err := parseIostat(iostatFixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (since-boot row dropped)", len(rows))
	}
	if rows[0].diskMBs["disk0"] != 2.93 || rows[1].diskMBs["disk4"] != 3.91 {
		t.Fatalf("disk MB/s = %+v %+v", rows[0].diskMBs, rows[1].diskMBs)
	}
	if *rows[0].cpuBusy != 30 || *rows[1].cpuBusy != 80 {
		t.Fatalf("cpu busy = %v %v", *rows[0].cpuBusy, *rows[1].cpuBusy)
	}
	if _, err := parseIostat("garbage\n"); err == nil {
		t.Fatal("no data rows must error")
	}
	one := strings.Join(strings.Split(iostatFixture, "\n")[:3], "\n")
	if _, err := parseIostat(one); err == nil {
		t.Fatal("only the since-boot row must error")
	}
}
