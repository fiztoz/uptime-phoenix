package provisioning

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// stageReceiptField is one canonical member of the helper receipt line. Values are
// already-encoded JSON so a case can mutate exactly one member.
type stageReceiptField struct{ key, value string }

// stageReceiptFields derives the only receipt the helper may emit for intent. It is
// written from the domain contract, not from the parser, so the two act as
// independent oracles.
func stageReceiptFields(intent domain.ProbeBinaryStageIntent) []stageReceiptField {
	pin := intent.Identity.Binary
	return []stageReceiptField{
		{"state", `"local_binary_staged"`},
		{"directory_name", strconv.Quote((domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName())},
		{"marker_sha256", strconv.Quote(intent.MarkerSHA256())},
		{"pinned_version", strconv.Quote(pin.Version)},
		{"version_source", `"operator_pin"`},
		{"os", `"linux"`},
		{"architecture", strconv.Quote(pin.Architecture)},
		{"sha256", strconv.Quote(pin.SHA256)},
		{"size", strconv.FormatInt(pin.Size, 10)},
	}
}

// stageReceiptIntentFixture is stageHeaderFixture with the root-only SSH user the
// delivery contract requires. Its marker therefore differs from the header
// fixture's marker for the same operation.
func stageReceiptIntentFixture(t *testing.T, max bool, arch string) domain.ProbeBinaryStageIntent {
	t.Helper()
	intent := stageHeaderFixture(t, max, arch)
	intent.Identity.Host.User = "root"
	if !intent.Valid() {
		t.Fatalf("root fixture invalid max=%v arch=%s", max, arch)
	}
	return intent
}

// stageReceiptObject renders one flat object with the given member/colon separators.
func stageReceiptObject(fields []stageReceiptField, between, colon string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteString(between)
		}
		b.WriteString(`"`)
		b.WriteString(f.key)
		b.WriteString(`"`)
		b.WriteString(colon)
		b.WriteString(f.value)
	}
	b.WriteByte('}')
	return b.String()
}

// stageReceiptLine is stageReceiptObject plus the mandatory single terminal LF.
func stageReceiptLine(fields []stageReceiptField, between, colon string) []byte {
	return []byte(stageReceiptObject(fields, between, colon) + "\n")
}

func assertStageReceiptAccepted(t *testing.T, body []byte, intent domain.ProbeBinaryStageIntent, label string) {
	t.Helper()
	ack, err := parseSSHBinaryStageAck(context.Background(), body, intent)
	if err != nil || ack.Intent != intent {
		t.Fatalf("%s: ack=%v err=%v, want exact ack for intent", label, ack, err)
	}
}

func assertStageReceiptRefused(t *testing.T, body []byte, intent domain.ProbeBinaryStageIntent, label string) {
	t.Helper()
	ack, err := parseSSHBinaryStageAck(context.Background(), body, intent)
	if err != ErrSSHBinaryStageUnknown || ack != (SSHBinaryStageAck{}) {
		t.Fatalf("%s: ack=%v err=%v, want zero ack and exact Unknown", label, ack, err)
	}
}

// stageReceiptNilContext is a nil-receiver context used to prove the typed-nil
// refusal. Its methods never dereference the receiver, so only an explicit guard
// can refuse it.
type stageReceiptNilContext struct{}

func (*stageReceiptNilContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*stageReceiptNilContext) Done() <-chan struct{}       { return nil }
func (*stageReceiptNilContext) Err() error                  { return nil }
func (*stageReceiptNilContext) Value(any) any               { return nil }

func TestParseSSHBinaryStageAckAcceptsCanonicalReceipt(t *testing.T) {
	intent := stageReceiptIntentFixture(t, false, "amd64")
	fields := stageReceiptFields(intent)
	body := stageReceiptLine(fields, ",", ":")

	// The fixture must look like the contract line, not just satisfy the parser.
	if !bytes.HasPrefix(body, []byte(`{"state":"local_binary_staged","directory_name":"uptime-phoenix-probe-`+intent.Identity.OperationID+`",`)) {
		t.Fatalf("fixture drifted: %q", body)
	}
	if !bytes.Contains(body, []byte(`"version_source":"operator_pin","os":"linux"`)) {
		t.Fatalf("fixture drift agent: %q", body)
	}
	if !bytes.Contains(body, []byte(`"pinned_version":"`+intent.Identity.Binary.Version+`","version_source"`)) {
		t.Fatalf("fixture drift pin: %q", body)
	}
	if !bytes.HasSuffix(body, []byte(`"size":9}`+"\n")) {
		t.Fatalf("fixture drift size: %q", body)
	}
	if bytes.Count(body, []byte{'\n'}) != 1 || bytes.ContainsRune(body, '\r') {
		t.Fatalf("fixture line shape: %q", body)
	}

	assertStageReceiptAccepted(t, body, intent, "canonical")

	// Order and interior JSON whitespace may vary.
	reversed := make([]stageReceiptField, len(fields))
	for i := range fields {
		reversed[len(fields)-1-i] = fields[i]
	}
	for _, tc := range []struct {
		name           string
		fields         []stageReceiptField
		between, colon string
	}{
		{"reversed", reversed, ",", ":"},
		{"spaced members", fields, " , ", ":"},
		{"spaced colons", reversed, ",", " : "},
		{"tabs", fields, "\t,\t", "\t:\t"},
		{"indented inside braces", reversed, ", ", ":"},
	} {
		assertStageReceiptAccepted(t, stageReceiptLine(tc.fields, tc.between, tc.colon), intent, tc.name)
	}

	// Every supported architecture and the largest pinned size.
	for _, arch := range []string{"amd64", "arm64"} {
		for _, max := range []bool{false, true} {
			item := stageReceiptIntentFixture(t, max, arch)
			line := stageReceiptLine(stageReceiptFields(item), ",", ":")
			assertStageReceiptAccepted(t, line, item, "arch "+arch+" max "+strconv.FormatBool(max))
		}
	}

	// A receipt produced for another operation must still bind to its own intent.
	other := stageReceiptIntentFixture(t, false, "arm64")
	other.Identity.OperationID = "44444444-4444-4444-8444-444444444444"
	other.OwnerID = "55555555-5555-4555-8555-555555555555"
	other.AttemptID = "66666666-6666-4666-8666-666666666666"
	if !other.Valid() {
		t.Fatal("second fixture invalid")
	}
	assertStageReceiptAccepted(t, stageReceiptLine(stageReceiptFields(other), ",", ":"), other, "second intent")
}

func TestParseSSHBinaryStageAckEnforcesByteBound(t *testing.T) {
	intent := stageReceiptIntentFixture(t, true, "arm64")
	object := stageReceiptObject(stageReceiptFields(intent), ",", ":")
	if object[0] != '{' || object[len(object)-1] != '}' {
		t.Fatalf("fixture braces: %q", object)
	}
	pad := MaxSSHBinaryStageReceiptSize - 1 - len(object)
	if pad < 1 {
		t.Fatalf("fixture too large to pad: %d", len(object))
	}
	atLimit := "{" + strings.Repeat(" ", pad) + object[1:]
	if len(atLimit)+1 != MaxSSHBinaryStageReceiptSize {
		t.Fatalf("padding %d produced %d", pad, len(atLimit)+1)
	}
	assertStageReceiptAccepted(t, []byte(atLimit+"\n"), intent, "exactly max bytes")

	overLimit := "{" + strings.Repeat(" ", pad+1) + object[1:]
	if len(overLimit)+1 != MaxSSHBinaryStageReceiptSize+1 {
		t.Fatalf("padding %d produced %d", pad+1, len(overLimit)+1)
	}
	assertStageReceiptRefused(t, []byte(overLimit+"\n"), intent, "one byte over max")
}

func TestParseSSHBinaryStageAckRejectsMisboundFields(t *testing.T) {
	intent := stageReceiptIntentFixture(t, false, "amd64")
	pin := intent.Identity.Binary
	other := stageReceiptIntentFixture(t, false, "arm64")
	other.Identity.OperationID = "44444444-4444-4444-8444-444444444444"
	other.OwnerID = "55555555-5555-4555-8555-555555555555"
	other.AttemptID = "66666666-6666-4666-8666-666666666666"
	if !other.Valid() || other.Identity.OperationID == intent.Identity.OperationID {
		t.Fatal("second fixture is not distinct")
	}
	op := intent.Identity.OperationID

	for _, tc := range []struct {
		name  string
		key   string
		value string
	}{
		{"state", "state", `"staged"`},
		{"state trailing space", "state", `"local_binary_staged "`},
		{"state case", "state", `"LOCAL_BINARY_STAGED"`},
		{"state empty", "state", `""`},
		{"directory absolute", "directory_name", strconv.Quote("/opt/uptime-phoenix-probe-" + op)},
		{"directory data dir", "directory_name", strconv.Quote("/var/lib/uptime-phoenix-probe-" + op)},
		{"directory other operation", "directory_name", strconv.Quote("uptime-phoenix-probe-" + other.Identity.OperationID)},
		{"directory empty", "directory_name", `""`},
		{"marker other intent", "marker_sha256", strconv.Quote(other.MarkerSHA256())},
		{"marker zeros", "marker_sha256", strconv.Quote(strings.Repeat("0", 64))},
		{"marker uppercase", "marker_sha256", strconv.Quote(strings.ToUpper(intent.MarkerSHA256()))},
		{"pinned version", "pinned_version", strconv.Quote("1.2.4")},
		{"pinned version latest", "pinned_version", strconv.Quote("latest")},
		{"pinned version release name", "pinned_version", strconv.Quote("phoenix-probe-" + pin.Version)},
		{"version source", "version_source", `"pin"`},
		{"version source latest", "version_source", `"latest"`},
		{"os", "os", `"darwin"`},
		{"os linux suffix", "os", `"linux2"`},
		{"architecture", "architecture", strconv.Quote("arm64")},
		{"architecture case", "architecture", strconv.Quote("AMD64")},
		{"sha256", "sha256", strconv.Quote(strings.Repeat("f", 64))},
		{"sha256 short", "sha256", strconv.Quote("00")},
		{"size plus one", "size", strconv.FormatInt(pin.Size+1, 10)},
		{"size minus one", "size", strconv.FormatInt(pin.Size-1, 10)},
		{"size zero", "size", "0"},
		{"size negative", "size", "-9"},
	} {
		fields := stageReceiptFields(intent)
		found := false
		for i := range fields {
			if fields[i].key == tc.key {
				if fields[i].value == tc.value {
					t.Fatalf("%s: mutation equals the canonical value", tc.name)
				}
				fields[i].value = tc.value
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: unknown fixture key", tc.name)
		}
		assertStageReceiptRefused(t, stageReceiptLine(fields, ",", ":"), intent, tc.name)
	}
}

func TestParseSSHBinaryStageAckRejectsStructuralMutations(t *testing.T) {
	intent := stageReceiptIntentFixture(t, false, "amd64")
	fields := stageReceiptFields(intent)
	raw := stageReceiptObject(fields, ",", ":")
	values := make(map[string]string, len(fields))
	for _, f := range fields {
		values[f.key] = f.value
	}

	for _, f := range fields {
		missing := make([]stageReceiptField, 0, len(fields)-1)
		for _, g := range fields {
			if g.key != f.key {
				missing = append(missing, g)
			}
		}
		assertStageReceiptRefused(t, stageReceiptLine(missing, ",", ":"), intent, "missing "+f.key)

		duplicate := append(append([]stageReceiptField{}, fields...), f)
		assertStageReceiptRefused(t, stageReceiptLine(duplicate, ",", ":"), intent, "duplicate "+f.key)

		// The duplicate must be refused even when the escaped spelling is the
		// only thing distinguishing it from the canonical member.
		escaped := strings.Replace(raw, `"`+f.key+`"`, `"`+stageReceiptEscapedKey(f.key)+`"`, 1)
		assertStageReceiptRefused(t, []byte(escaped+"\n"), intent, "escaped key "+f.key)

		aliased := strings.Replace(raw, `"`+f.key+`"`, `"`+stageReceiptTitledKey(f.key)+`"`, 1)
		assertStageReceiptRefused(t, []byte(aliased+"\n"), intent, "case alias "+f.key)

		nulled := strings.Replace(raw, `"`+f.key+`":`+values[f.key], `"`+f.key+`":null`, 1)
		assertStageReceiptRefused(t, []byte(nulled+"\n"), intent, "null "+f.key)

		spaced := strings.Replace(raw, `"`+f.key+`"`, `"`+f.key+` "`, 1)
		assertStageReceiptRefused(t, []byte(spaced+"\n"), intent, "key trailing space "+f.key)

		// Each mutation above must actually differ from the canonical object, or a
		// negative case would silently assert the acceptance path.
		for _, changed := range []string{escaped, aliased, nulled, spaced} {
			if changed == raw {
				t.Fatalf("%s: mutation did not change the receipt", f.key)
			}
		}
	}

	assertStageReceiptRefused(t, []byte(strings.TrimSuffix(raw, "}")+`,"state":"local_binary_staged"}`+"\n"), intent, "unknown member")
	assertStageReceiptRefused(t, []byte(strings.TrimSuffix(raw, "}")+`,"":1}`+"\n"), intent, "empty key member")
	assertStageReceiptRefused(t, []byte(raw+"{}\n"), intent, "trailing object")
	assertStageReceiptRefused(t, []byte(raw+"null\n"), intent, "trailing null")
	assertStageReceiptRefused(t, []byte(raw), intent, "no terminal LF")
	assertStageReceiptRefused(t, []byte(raw+"\n\n"), intent, "second LF")
	assertStageReceiptRefused(t, []byte("\n"+raw+"\n"), intent, "leading LF")
	assertStageReceiptRefused(t, []byte(" "+raw+"\n"), intent, "leading space")
	assertStageReceiptRefused(t, []byte(raw+" \n"), intent, "trailing space")
	assertStageReceiptRefused(t, []byte("\t"+raw+"\n"), intent, "leading tab")
	assertStageReceiptRefused(t, []byte(strings.Replace(raw, "{", "{\r", 1)+"\n"), intent, "internal CR")
	assertStageReceiptRefused(t, []byte(raw[:1]+"\n"+raw[1:]+"\n"), intent, "internal LF")
	assertStageReceiptRefused(t, []byte{}, intent, "empty body")
	assertStageReceiptRefused(t, []byte("\n"), intent, "LF only")

	invalid := bytes.Replace([]byte(raw), []byte(`"linux"`), []byte{'"', 0xff, '"'}, 1)
	if bytes.Equal(invalid, []byte(raw)) {
		t.Fatal("invalid utf8 fixture did not change")
	}
	assertStageReceiptRefused(t, append(invalid, '\n'), intent, "invalid UTF-8")

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"nested object state", []byte(strings.Replace(raw, `"state":"local_binary_staged"`, `"state":{"state":"local_binary_staged"}`, 1) + "\n")},
		{"nested array size", []byte(strings.Replace(raw, `"size":9`, `"size":[9]`, 1) + "\n")},
		{"state number", []byte(strings.Replace(raw, `"state":"local_binary_staged"`, `"state":1`, 1) + "\n")},
		{"state bool", []byte(strings.Replace(raw, `"state":"local_binary_staged"`, `"state":true`, 1) + "\n")},
		{"size string", []byte(strings.Replace(raw, `"size":9`, `"size":"9"`, 1) + "\n")},
		{"size bool", []byte(strings.Replace(raw, `"size":9`, `"size":true`, 1) + "\n")},
		{"array body", []byte("[" + raw[1:len(raw)-1] + "]\n")},
		{"top level string", []byte(`"local_binary_staged"` + "\n")},
		{"two objects", []byte(raw + raw + "\n")},
	} {
		if bytes.Equal(tc.body, []byte(raw+"\n")) {
			t.Fatalf("%s: fixture did not change", tc.name)
		}
		assertStageReceiptRefused(t, tc.body, intent, tc.name)
	}

	for _, spelling := range []string{"007", "+9", "-0", "9.0", "9e0", "0x9", "--9", "9-", "09"} {
		body := []byte(strings.Replace(raw, `"size":9`, `"size":`+spelling, 1) + "\n")
		if bytes.Equal(body, []byte(raw+"\n")) {
			t.Fatalf("size spelling %q: fixture did not change", spelling)
		}
		assertStageReceiptRefused(t, body, intent, "noncanonical size "+spelling)
	}
}

func TestParseSSHBinaryStageAckRefusesBadContextAndIntent(t *testing.T) {
	intent := stageReceiptIntentFixture(t, false, "amd64")
	body := stageReceiptLine(stageReceiptFields(intent), ",", ":")

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	expiredCtx, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	var typedNil *stageReceiptNilContext

	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"nil context", nil},
		{"typed nil context", typedNil},
		{"canceled context", canceledCtx},
		{"expired context", expiredCtx},
	} {
		ack, err := parseSSHBinaryStageAck(tc.ctx, body, intent)
		if err != ErrSSHBinaryStageUnknown || ack != (SSHBinaryStageAck{}) {
			t.Fatalf("%s: ack=%v err=%v, want zero ack and exact Unknown", tc.name, ack, err)
		}
	}

	live, cancelLive := context.WithCancel(context.Background())
	defer cancelLive()
	ack, err := parseSSHBinaryStageAck(live, body, intent)
	if err != nil || ack.Intent != intent {
		t.Fatalf("live context: ack=%v err=%v", ack, err)
	}

	docker := stageHeaderFixture(t, false, "amd64")
	docker.Identity.Mode = domain.ProbeInstallDocker

	for _, tc := range []struct {
		name   string
		intent domain.ProbeBinaryStageIntent
	}{
		{"zero intent", domain.ProbeBinaryStageIntent{}},
		{"empty owner", func() domain.ProbeBinaryStageIntent {
			i := stageReceiptIntentFixture(t, false, "amd64")
			i.OwnerID = ""
			return i
		}()},
		{"owner uppercase", func() domain.ProbeBinaryStageIntent {
			i := stageReceiptIntentFixture(t, false, "amd64")
			i.OwnerID = "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"
			return i
		}()},
		{"empty attempt", func() domain.ProbeBinaryStageIntent {
			i := stageReceiptIntentFixture(t, false, "amd64")
			i.AttemptID = ""
			return i
		}()},
		{"docker mode", docker},
		// The delivery contract is root-only: a valid non-root intent can never
		// have produced this receipt, so it is refused rather than acknowledged.
		{"non-root user", func() domain.ProbeBinaryStageIntent {
			i := stageReceiptIntentFixture(t, false, "amd64")
			i.Identity.Host.User = "ubuntu"
			if !i.Valid() {
				t.Fatal("non-root fixture invalid")
			}
			return i
		}()},
	} {
		ack, err := parseSSHBinaryStageAck(context.Background(), stageReceiptLine(stageReceiptFields(tc.intent), ",", ":"), tc.intent)
		if err != ErrSSHBinaryStageUnknown || ack != (SSHBinaryStageAck{}) {
			t.Fatalf("%s: ack=%v err=%v, want zero ack and exact Unknown", tc.name, ack, err)
		}
		// A structurally perfect receipt for a valid intent is still refused
		// when the supplied intent is not the one the transport may attempt.
		ack, err = parseSSHBinaryStageAck(context.Background(), body, tc.intent)
		if err != ErrSSHBinaryStageUnknown || ack != (SSHBinaryStageAck{}) {
			t.Fatalf("%s with foreign receipt: ack=%v err=%v", tc.name, ack, err)
		}
	}
}

// stageReceiptEscapedKey spells key with a \u escape for its first character,
// which decoding resolves to the same key while canonical spelling does not.
func stageReceiptEscapedKey(key string) string {
	if key == "" {
		return key
	}
	return fmt.Sprintf("\\u%04x", rune(key[0])) + key[1:]
}

// stageReceiptTitledKey upper-cases only the first byte, the case alias
// encoding/json accepts.
func stageReceiptTitledKey(key string) string {
	if key == "" {
		return key
	}
	return strings.ToUpper(key[:1]) + key[1:]
}
