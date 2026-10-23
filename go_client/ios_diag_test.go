package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseWGStatsFromUAPI(t *testing.T) {
	now := time.Unix(1_800_000_100, 0)
	uapi := `private_key=SECRET
listen_port=51820
public_key=PEERKEY
last_handshake_time_sec=1800000040
last_handshake_time_nsec=0
tx_bytes=12345
rx_bytes=67890
persistent_keepalive_interval=25
errno=0
`

	handshake, rx, tx, ok := parseWGStats(uapi, now)
	if !ok {
		t.Fatalf("ожидался ok=true")
	}
	if rx != 67890 || tx != 12345 {
		t.Fatalf("счётчики: rx=%d tx=%d", rx, tx)
	}
	if got := handshake.Unix(); got != 1_800_000_040 {
		t.Fatalf("handshake=%d", got)
	}
	if age := now.Sub(handshake); age != 60*time.Second {
		t.Fatalf("age=%v", age)
	}
}

func TestParseWGStatsEmptyPeer(t *testing.T) {
	_, _, _, ok := parseWGStats("private_key=SECRET\nlisten_port=51820\nerrno=0\n", time.Now())
	if ok {
		t.Fatalf("без peer-блока ok должен быть false")
	}
}

func TestDiagSnapshotHasNoSecrets(t *testing.T) {
	s := diagSnapshot{
		Reason:         "test",
		At:             time.Unix(1_800_000_000, 0),
		Build:          "203",
		Generation:     2,
		ConfigReady:    true,
		WGReady:        true,
		WGHandshakeAge: 30 * time.Second,
		WGRxBytes:      1,
		WGTxBytes:      2,
		TunUpBytes:     3,
		TunDownBytes:   4,
		DtlsTxBytes:    5,
		DtlsRxBytes:    6,
		Workers:        70,
		Active:         70,
		Physical:       "OK(12ms)",
		Repairs:        1,
	}.String()

	for _, forbidden := range []string{"SECRET", "password", "PrivateKey", "public_key"} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("снимок содержит запрещённое поле %q: %s", forbidden, s)
		}
	}
	for _, required := range []string{"[ДИАГ-СНИМОК]", "gen=2", "cfg=1", "wg=1", "workers=70", "active=70", "phys=OK"} {
		if !strings.Contains(s, required) {
			t.Fatalf("в снимке нет %q: %s", required, s)
		}
	}
}

func TestRepairPolicyCooldownAndLimit(t *testing.T) {
	policy := repairPolicy{cooldown: 60 * time.Second, window: 10 * time.Minute, max: 3}
	now := time.Unix(1_800_000_000, 0)

	allowed, why := policy.allow(now, time.Time{}, 0, time.Time{})
	if !allowed || why != "" {
		t.Fatalf("первый ремонт должен быть разрешён: %v %q", allowed, why)
	}

	allowed, why = policy.allow(now.Add(10*time.Second), now, 1, now)
	if allowed {
		t.Fatalf("кулдаун не сработал: %q", why)
	}

	attempts, start := repairWindowState(now, time.Time{}, 0, policy)
	if attempts != 1 || !start.Equal(now) {
		t.Fatalf("окно: attempts=%d start=%v", attempts, start)
	}
	attempts, _ = repairWindowState(now.Add(10*time.Second), start, attempts, policy)
	if attempts != 2 {
		t.Fatalf("attempts=%d", attempts)
	}
	attempts, _ = repairWindowState(now.Add(20*time.Second), start, attempts, policy)
	if attempts != 3 {
		t.Fatalf("attempts=%d", attempts)
	}

	allowed, why = policy.allow(now.Add(5*time.Minute), now.Add(20*time.Second), 3, start)
	if allowed {
		t.Fatalf("лимит не сработал: %q", why)
	}

	allowed, _ = policy.allow(now.Add(11*time.Minute), now.Add(20*time.Second), 3, start)
	if !allowed {
		t.Fatalf("после окна ремонт снова должен быть разрешён")
	}
}

func TestDataPlaneStale(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	maxAge := 150 * time.Second

	if dataPlaneStale(now, time.Time{}, 1000, 1000, maxAge) {
		t.Fatalf("без прироста TX тревога не нужна")
	}
	if !dataPlaneStale(now, time.Time{}, 2000, 1000, maxAge) {
		t.Fatalf("TX растёт, handshake нет — должен быть stale")
	}
	if dataPlaneStale(now, now.Add(-30*time.Second), 2000, 1000, maxAge) {
		t.Fatalf("свежий handshake — не stale")
	}
	if !dataPlaneStale(now, now.Add(-5*time.Minute), 2000, 1000, maxAge) {
		t.Fatalf("старый handshake при росте TX — stale")
	}
}
