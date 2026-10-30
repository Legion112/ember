package node

import (
	"strings"
	"testing"
)

func TestStoreHandle(t *testing.T) {
	h := StoreHandle{Provider: "runpod", ID: "vol_abc"}
	if got, want := h.String(), "runpod/vol_abc"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if h.IsZero() {
		t.Error("a populated handle reported IsZero")
	}
	if !(StoreHandle{}).IsZero() {
		t.Error("the zero handle did not report IsZero")
	}
	// Half a handle is not zero. A handle with a provider and no id names a
	// provider ember cannot ask about anything, which is a bug worth seeing
	// rather than a value that quietly reads as "nothing here".
	if (StoreHandle{Provider: "runpod"}).IsZero() {
		t.Error("a handle with a provider and no id reported IsZero")
	}
}

func TestStore_String(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store Store
		want  string
	}{
		{
			name:  "with a region",
			store: Store{Handle: StoreHandle{"runpod", "v1"}, Name: "models", SizeGiB: 40, Region: "EU-RO-1"},
			want:  "models (runpod/v1, 40 GiB, EU-RO-1)",
		},
		{
			// A provider that does not pin stores to a region says so in
			// words. An empty field here would read as a missing value.
			name:  "without a region",
			store: Store{Handle: StoreHandle{"local", "v2"}, Name: "lab", SizeGiB: 10},
			want:  "lab (local/v2, 10 GiB, anywhere)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.store.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStorePrice_String(t *testing.T) {
	// $0.07/GiB/month, the rate RunPod charges below 1 TB.
	if got, want := StorePrice(70_000).String(), "$0.07/GiB/month"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestStorePrice_Monthly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		price StorePrice
		size  int
		want  MicroUSD
	}{
		{"the 40 GiB case this work exists for", 70_000, 40, 2_800_000},
		{"the smallest store RunPod will sell", 70_000, MinStoreGiB, 700_000},
		{"free storage costs nothing", 0, 40, 0},
		{"a negative price is not a refund", -1, 40, 0},
		{"no size, no charge", 70_000, 0, 0},
		{"a negative size is not a credit", 70_000, -40, 0},
		{
			// Saturates rather than wrapping, for the reason Rate.Cost does:
			// a negative cost compares as under budget, and the one place
			// that must never happen is the check standing between a typo and
			// a bill.
			name:  "an absurd price saturates instead of going negative",
			price: StorePrice(MaxMicroUSD),
			size:  2,
			want:  MaxMicroUSD,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.price.Monthly(tc.size); got != tc.want {
				t.Errorf("Monthly(%d) = %d, want %d", tc.size, got, tc.want)
			}
		})
	}
}

func TestStorePrice_Hourly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		price StorePrice
		size  int
		want  Rate
	}{
		{
			// 700000 / 730 = 958.90, and this must not report 958. A cost
			// that rounds down is a bill that arrives larger than the
			// estimate, which is the direction that loses money quietly.
			name:  "rounds up rather than truncating",
			price: 70_000, size: 10, want: 959,
		},
		{"40 GiB at RunPod's rate", 70_000, 40, 3836},
		{"free storage", 0, 40, 0},
		{"no size", 70_000, 0, 0},
		{"an overflowing monthly cost saturates", StorePrice(MaxMicroUSD), 2, MaxRate},
		{
			// Reachable without overflowing Monthly: a very large but
			// representable monthly cost still exceeds the hourly ceiling.
			name:  "an hourly rate above the ceiling saturates",
			price: 1_000_000_000, size: 1000, want: MaxRate,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.price.Hourly(tc.size); got != tc.want {
				t.Errorf("Hourly(%d) = %d, want %d", tc.size, got, tc.want)
			}
		})
	}
}

func TestStorePrice_HourlyIsNeverLessThanMonthlyImplies(t *testing.T) {
	// A property rather than a case: for every size ember will accept, the
	// hourly rate billed for a month must cover the monthly price. The
	// rounding direction is the whole point, so it is asserted over the range
	// rather than at one convenient value.
	const price = StorePrice(70_000)
	for size := MinStoreGiB; size <= MaxStoreGiB; size++ {
		monthly := price.Monthly(size)
		hourly := price.Hourly(size)
		if got := MicroUSD(hourly) * HoursPerMonth; got < monthly {
			t.Fatalf("size %d GiB: hourly %d over %d hours = %d, which is below the monthly %d",
				size, hourly, HoursPerMonth, got, monthly)
		}
	}
}

func TestMount_Zero(t *testing.T) {
	var m Mount
	if !m.IsZero() {
		t.Error("the zero Mount did not report IsZero")
	}
	if m.Path() != "" {
		t.Errorf("the zero Mount has path %q", m.Path())
	}
	if got, want := m.String(), "(no mount)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestMountAt_Accepts(t *testing.T) {
	for _, p := range []string{"/models", "/mnt/models", "/opt/weights", "/data"} {
		t.Run(p, func(t *testing.T) {
			m, err := MountAt(p)
			if err != nil {
				t.Fatalf("MountAt(%q) = %v", p, err)
			}
			if m.Path() != p {
				t.Errorf("Path() = %q, want %q", m.Path(), p)
			}
			if m.IsZero() {
				t.Error("a valid mount reported IsZero")
			}
			if m.String() != p {
				t.Errorf("String() = %q, want %q", m.String(), p)
			}
		})
	}
}

func TestMountAt_Refuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		wantMsg string
	}{
		{"empty", "", "empty"},
		{"relative", "models", "relative"},
		{"relative with a dot", "./models", "relative"},
		{
			// Refused, not cleaned. "/models/.." and "/" are different
			// requests and an operator who typed the first is owed an error
			// rather than the second.
			name: "unclean", path: "/models/..", wantMsg: "simplest form",
		},
		{"trailing slash is unclean", "/models/", "simplest form"},
		{"root", "/", "shadow the whole node"},
		{
			// The expensive one: RunPod's default mount path is also where
			// ember writes outputs, so a store there bills for every render
			// for as long as the store exists.
			name: "workspace", path: "/workspace", wantMsg: "bills for every render",
		},
		{"under workspace", "/workspace/out", "bills for every render"},
		{
			// The silent one: shadowing these leaves sshd with no host key
			// and no authorized_keys, and the handshake failure reads exactly
			// like a host-key attack.
			name: "root home", path: "/root", wantMsg: "nothing can log into",
		},
		{"under root home", "/root/.ssh", "nothing can log into"},
		{"etc", "/etc", "nothing can log into"},
		{"under etc", "/etc/ssh", "nothing can log into"},
		{"usr", "/usr", "system directory"},
		{"under var", "/var/lib", "system directory"},
		{"proc", "/proc", "system directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := MountAt(tc.path)
			if err == nil {
				t.Fatalf("MountAt(%q) was accepted as %q", tc.path, m.Path())
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("MountAt(%q) = %q, want it to mention %q", tc.path, err, tc.wantMsg)
			}
			if !m.IsZero() {
				t.Errorf("a refused mount returned a usable Mount %q", m.Path())
			}
		})
	}
}

func TestMountAt_PrefixMatchIsOnASeparator(t *testing.T) {
	// /rootfs is not /root, and /workspaces is not /workspace. The same
	// boundary bug GPUModel.Accepts exists to prevent, reached from the other
	// direction: a prefix match without the separator would refuse paths that
	// are perfectly safe, and the operator would have no idea why.
	for _, p := range []string{"/rootfs", "/workspaces", "/etcetera", "/variable"} {
		t.Run(p, func(t *testing.T) {
			if _, err := MountAt(p); err != nil {
				t.Errorf("MountAt(%q) = %v, want it accepted", p, err)
			}
		})
	}
}

func TestMountAt_RefusesEveryListedPath(t *testing.T) {
	// The list is the control, so the test walks it rather than naming a few
	// members. A path added to refusedMountPaths without a reason in
	// whyRefused would otherwise fall through to the default message and
	// nobody would notice.
	for _, p := range refusedMountPaths {
		if _, err := MountAt(p); err == nil {
			t.Errorf("MountAt(%q) was accepted although it is on the refusal list", p)
		}
		if why := whyRefused(p); why == "" {
			t.Errorf("whyRefused(%q) is empty", p)
		}
	}
}
