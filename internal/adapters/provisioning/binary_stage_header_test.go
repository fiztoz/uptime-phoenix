package provisioning

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestProbeBinaryStageHeaderRoundTrip(t *testing.T) {
	if ProbeBinaryStagePrefixSize != 12 || MaxProbeBinaryStageHeaderSize != 4096 {
		t.Fatalf("prefix=%d max=%d", ProbeBinaryStagePrefixSize, MaxProbeBinaryStageHeaderSize)
	}
	intent := stageHeaderFixture(t, false, "amd64")
	payload := stageHeaderLiteral
	if got := testStageHeaderJSON(intent); got != payload {
		t.Fatalf("oracle drifted:\n got %s\nwant %s", got, payload)
	}
	framed := stageHeaderFrame(t, []byte(payload))
	decoded, err := DecodeProbeBinaryStageHeader(framed)
	assertStageHeaderIntent(t, decoded, err, intent)
	encoded, err := EncodeProbeBinaryStageHeader(intent)
	if err != nil || !bytes.Equal(encoded, framed) {
		t.Fatalf("encode = %q, %v", encoded, err)
	}
	if n, err := ProbeBinaryStageHeaderLength(encoded[:ProbeBinaryStagePrefixSize]); err != nil || n != len(payload) {
		t.Fatalf("length = %d, %v", n, err)
	}
	again, err := DecodeProbeBinaryStageHeader(encoded)
	assertStageHeaderIntent(t, again, err, intent)

	for _, arch := range []string{"amd64", "arm64"} {
		for _, max := range []bool{false, true} {
			item := stageHeaderFixture(t, max, arch)
			raw := testStageHeaderJSON(item)
			got, err := DecodeProbeBinaryStageHeader(stageHeaderFrame(t, []byte(raw)))
			assertStageHeaderIntent(t, got, err, item)
			out, err := EncodeProbeBinaryStageHeader(item)
			if err != nil || !bytes.Equal(out, stageHeaderFrame(t, []byte(raw))) {
				t.Fatalf("arch %s max %v encode mismatch: %v", arch, max, err)
			}
			round, err := DecodeProbeBinaryStageHeader(out)
			assertStageHeaderIntent(t, round, err, item)
		}
	}

	ipv6 := stageHeaderFixture(t, false, "arm64")
	ipv6.Identity.Host.Address = "[2001:db8::1]:22"
	if !ipv6.Valid() {
		t.Fatal("ipv6 fixture")
	}
	raw := testStageHeaderJSON(ipv6)
	got, err := DecodeProbeBinaryStageHeader(stageHeaderFrame(t, []byte(raw)))
	assertStageHeaderIntent(t, got, err, ipv6)
	if !bytes.Contains([]byte(raw), []byte("[2001:db8::1]")) {
		t.Fatal("ipv6 brackets lost")
	}

	mixed := stageHeaderFixture(t, false, "amd64")
	mixed.Identity.HostKeyFingerprint = stageHeaderFingerprint(0xff)
	mixed.Identity.ClientKeyFingerprint = stageHeaderFingerprint(0xfb)
	if !mixed.Valid() {
		t.Fatal("mixed fingerprint fixture")
	}
	mixedRaw := testStageHeaderJSON(mixed)
	got, err = DecodeProbeBinaryStageHeader(stageHeaderFrame(t, []byte(mixedRaw)))
	assertStageHeaderIntent(t, got, err, mixed)
	out, err := EncodeProbeBinaryStageHeader(mixed)
	if err != nil || !bytes.Contains(out, []byte(mixed.Identity.HostKeyFingerprint)) || bytes.Contains(out, []byte(`\u00`)) {
		t.Fatalf("fingerprint escaped: %q %v", out, err)
	}
}

func TestProbeBinaryStageHeaderDTOBindsEveryField(t *testing.T) {
	raw, err := json.Marshal(probeBinaryStageHeaderDTO{})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || len(obj) != probeBinaryStageHeaderKeyCount {
		t.Fatalf("dto keys: %s", raw)
	}
	prev := -1
	for _, key := range []string{
		"operation_id", "owner_id", "attempt_id", "ssh_address", "ssh_user",
		"host_key_fingerprint", "client_key_fingerprint", "requested_by",
		"layout_version", "mode", "version", "architecture", "sha256", "size",
	} {
		i := strings.Index(string(raw), `"`+key+`"`)
		if _, ok := obj[key]; !ok || i <= prev {
			t.Fatalf("key %s missing or unordered", key)
		}
		prev = i
	}
	if strings.Contains(string(raw), "omitempty") {
		t.Fatal("tag leaked")
	}
}

func TestProbeBinaryStageHeaderLengthAndPrefix(t *testing.T) {
	payload := []byte(stageHeaderLiteral)
	valid := stageHeaderFrame(t, payload)
	if n, err := ProbeBinaryStageHeaderLength(valid[:12]); err != nil || n != len(payload) {
		t.Fatalf("valid length %d %v", n, err)
	}
	cases := [][]byte{
		nil,
		{},
		bytes.Repeat([]byte{0xff}, 11),
		bytes.Repeat([]byte{0xff}, 13),
		stageHeaderPrefix("PHXSTG0\n", 1),
		stageHeaderPrefix("phxstg1\n", 1),
		stageHeaderPrefix("PHXSTG1\x00", 1),
		stageHeaderPrefix("PHXSTG1\n", 0),
		stageHeaderPrefix("PHXSTG1\n", 4097),
		stageHeaderPrefix("PHXSTG1\n", 0xffffffff),
		append(append([]byte{}, valid[:8]...), valid[9:12]...),
	}
	for i, prefix := range cases {
		n, err := ProbeBinaryStageHeaderLength(prefix)
		if n != 0 || err != domain.ErrProbeBinaryStageWriteInput {
			t.Fatalf("prefix %d: %d %v", i, n, err)
		}
		got, err := DecodeProbeBinaryStageHeader(prefix)
		assertStageHeaderRefused(t, got, err)
	}
	short := valid[:len(valid)-1]
	declared := binary.BigEndian.Uint32(short[8:12])
	if int(declared) == len(short)-12 {
		t.Fatal("truncation did not desynchronize length")
	}
	got, err := DecodeProbeBinaryStageHeader(short)
	assertStageHeaderRefused(t, got, err)

	extra := append(append([]byte{}, valid...), 'X')
	got, err = DecodeProbeBinaryStageHeader(extra)
	assertStageHeaderRefused(t, got, err)

	shifted := append([]byte{}, valid...)
	binary.BigEndian.PutUint32(shifted[8:12], uint32(len(payload)+1))
	shifted = append(shifted, 'X')
	got, err = DecodeProbeBinaryStageHeader(shifted)
	assertStageHeaderRefused(t, got, err)

	spaced := strings.Replace(stageHeaderLiteral, `"size":9`, `"size": 9`, 1)
	got, err = DecodeProbeBinaryStageHeader(stageHeaderFrame(t, []byte(spaced)))
	assertStageHeaderIntent(t, got, err, stageHeaderFixture(t, false, "amd64"))
	if bytes.Contains(mustStageHeaderEncode(t, stageHeaderFixture(t, false, "amd64")), []byte(`"size": 9`)) {
		t.Fatal("encoder emitted noncanonical spacing")
	}
}

func TestDecodeProbeBinaryStageHeaderRejects(t *testing.T) {
	base := stageHeaderLiteral
	replace := func(old, new string) []byte {
		t.Helper()
		if strings.Count(base, old) != 1 {
			t.Fatalf("token %q count %d", old, strings.Count(base, old))
		}
		return stageHeaderFrame(t, []byte(strings.Replace(base, old, new, 1)))
	}
	cases := map[string][]byte{
		"missing_key":        stageHeaderFrame(t, []byte(strings.Replace(base, `"sha256":"`+strings.Repeat("0", 64)+`",`, "", 1))),
		"extra_key":          replace(`"mode":"systemd"`, `"mode":"systemd","sensitive-stage-marker":true`),
		"duplicate_key":      replace(`"operation_id":`, `"operation_id":"11111111-1111-4111-8111-111111111111","operation_id":`),
		"escaped_duplicate":  replace(`"owner_id":`, `"owner_id":"22222222-2222-4222-8222-222222222222","\u006fwner_id":`),
		"escaped_key":        replace(`"operation_id"`, `"\u006fperation_id"`),
		"case_alias":         replace(`"operation_id"`, `"Operation_ID"`),
		"case_alias_address": replace(`"ssh_address"`, `"SSH_Address"`),
		"null_size":          replace(`"size":9`, `"size":null`),
		"null_user":          replace(`"ssh_user":"root"`, `"ssh_user":null`),
		"bool_size":          replace(`"size":9`, `"size":true`),
		"array_sha":          replace(`"sha256":"`+strings.Repeat("0", 64)+`"`, `"sha256":["ab"]`),
		"object_mode":        replace(`"mode":"systemd"`, `"mode":{"systemd":true}`),
		"string_size":        replace(`"size":9`, `"size":"9"`),
		"number_mode":        replace(`"mode":"systemd"`, `"mode":1`),
		"float_size":         replace(`"size":9`, `"size":9.0`),
		"exponent":           replace(`"requested_by":7`, `"requested_by":7e0`),
		"overflow_size":      replace(`"size":9`, `"size":9223372036854775808`),
		"overflow_negative":  replace(`"requested_by":7`, `"requested_by":-9223372036854775809`),
		"negative_zero":      replace(`"size":9`, `"size":-0`),
		"leading_zero":       replace(`"size":9`, `"size":09`),
		"plus_number":        replace(`"requested_by":7`, `"requested_by":+7`),
		"docker_mode":        replace(`"mode":"systemd"`, `"mode":"docker"`),
		"mode_case":          replace(`"mode":"systemd"`, `"mode":"Systemd"`),
		"empty_mode":         replace(`"mode":"systemd"`, `"mode":""`),
		"layout_zero":        replace(`"layout_version":1`, `"layout_version":0`),
		"layout_two":         replace(`"layout_version":1`, `"layout_version":2`),
		"layout_negative":    replace(`"layout_version":1`, `"layout_version":-1`),
		"arch_case":          replace(`"architecture":"amd64"`, `"architecture":"AMD64"`),
		"arch_other":         replace(`"architecture":"amd64"`, `"architecture":"386"`),
		"sha_upper":          replace(strings.Repeat("0", 64), strings.Repeat("A", 64)),
		"sha_short":          replace(strings.Repeat("0", 64), strings.Repeat("0", 63)),
		"sha_nonhex":         replace(strings.Repeat("0", 64), strings.Repeat("g", 64)),
		"version_empty":      replace(`"version":"1.2.3"`, `"version":""`),
		"version_prefix":     replace(`"version":"1.2.3"`, `"version":"v1.2.3"`),
		"size_zero":          replace(`"size":9`, `"size":0`),
		"size_negative":      replace(`"size":9`, `"size":-1`),
		"size_over_max":      replace(`"size":9`, `"size":134217729`),
		"requested_zero":     replace(`"requested_by":7`, `"requested_by":0`),
		"requested_negative": replace(`"requested_by":7`, `"requested_by":-1`),
		"bad_uuid":           replace(`11111111-1111-4111-8111-111111111111`, `11111111-1111-4111-8111-11111111111G`),
		"local_owner":        replace(`22222222-2222-4222-8222-222222222222`, domain.LocalProbeID),
		"nil_attempt":        replace(`33333333-3333-4333-8333-333333333333`, `00000000-0000-0000-0000-000000000000`),
		"user_empty":         replace(`"ssh_user":"root"`, `"ssh_user":""`),
		"user_long":          replace(`"ssh_user":"root"`, `"ssh_user":"`+strings.Repeat("u", 65)+`"`),
		"user_space":         replace(`"ssh_user":"root"`, `"ssh_user":"sensitive-stage-marker x"`),
		"address_noport":     replace(`"ssh_address":"127.0.0.1:22"`, `"ssh_address":"127.0.0.1"`),
		"fingerprint_pad":    replace(`"host_key_fingerprint":"`+stageHeaderFingerprint(0)+`"`, `"host_key_fingerprint":"`+stageHeaderFingerprint(0)+`=="`),
		"fingerprint_prefix": replace(`"host_key_fingerprint":"SHA256:`, `"host_key_fingerprint":"sha256:`),
		"trailing_json":      stageHeaderFrame(t, []byte(base+`{}`)),
		"trailing_junk":      stageHeaderFrame(t, []byte(base+`X`)),
		"utf8":               stageHeaderFrame(t, append(append([]byte(base[:strings.Index(base, "root")]), 0xff), []byte(base[strings.Index(base, "root")+4:])...)),
		"bom":                stageHeaderFrame(t, append([]byte{0xef, 0xbb, 0xbf}, []byte(base)...)),
		"top_array":          stageHeaderFrame(t, []byte(`[`+base+`]`)),
		"top_null":           stageHeaderFrame(t, []byte(`null`)),
		"nested_hostile":     stageHeaderFrame(t, bytes.Repeat([]byte("{"), 4096)),
		"long_number":        replace(`"size":9`, `"size":`+strings.Repeat("9", 3000)),
		"bad_escape":         replace(`"ssh_user":"root"`, `"ssh_user":"\u12"`),
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := DecodeProbeBinaryStageHeader(header)
			assertStageHeaderRefused(t, got, err)
			if strings.Contains(err.Error(), "sensitive-stage-marker") || err.Error() != domain.ErrProbeBinaryStageWriteInput.Error() {
				t.Fatalf("diagnostic leaked: %q", err)
			}
		})
	}
}

func TestEncodeProbeBinaryStageHeaderRefusesIncomplete(t *testing.T) {
	base := stageHeaderFixture(t, false, "amd64")
	cases := []domain.ProbeBinaryStageIntent{
		{},
		func() domain.ProbeBinaryStageIntent {
			bad := base
			bad.Identity.Mode = domain.ProbeInstallDocker
			return bad
		}(),
		func() domain.ProbeBinaryStageIntent {
			bad := base
			bad.Identity.Image.LayerCount = 1
			return bad
		}(),
		func() domain.ProbeBinaryStageIntent {
			bad := base
			bad.OwnerID = "sensitive-stage-marker"
			return bad
		}(),
	}
	for i, intent := range cases {
		out, err := EncodeProbeBinaryStageHeader(intent)
		if out != nil || err != domain.ErrProbeBinaryStageWriteInput || strings.Contains(err.Error(), "sensitive-stage-marker") {
			t.Fatalf("case %d: %q %v", i, out, err)
		}
	}
	first := mustStageHeaderEncode(t, base)
	second := mustStageHeaderEncode(t, base)
	if !bytes.Equal(first, second) {
		t.Fatal("encode is not deterministic")
	}
	if !bytes.HasPrefix(first, []byte("PHXSTG1\n")) || len(first) != ProbeBinaryStagePrefixSize+len(stageHeaderLiteral) {
		t.Fatal("prefix or payload contamination")
	}
}

func TestProbeBinaryStageHeaderHostilePrefixNoExcessAlloc(t *testing.T) {
	small := bytes.Repeat([]byte{0xff}, ProbeBinaryStagePrefixSize)
	large := bytes.Repeat([]byte{0xff}, 1<<20)
	measure := func(prefix []byte) float64 {
		return testing.AllocsPerRun(50, func() {
			_, _ = ProbeBinaryStageHeaderLength(prefix)
			_, _ = DecodeProbeBinaryStageHeader(prefix)
		})
	}
	if measure(large) > measure(small) {
		t.Fatalf("hostile prefix allocated more than a fixed prefix: large %v small %v", measure(large), measure(small))
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("panic: %v", recovered)
		}
	}()
	hugeN := stageHeaderPrefix("PHXSTG1\n", 0xffffffff)
	got, err := DecodeProbeBinaryStageHeader(hugeN)
	assertStageHeaderRefused(t, got, err)
}

func assertStageHeaderIntent(t *testing.T, got domain.ProbeBinaryStageIntent, err error, want domain.ProbeBinaryStageIntent) {
	t.Helper()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Identity.Binary != want.Identity.Binary {
		t.Fatalf("pin got %#v want %#v", got.Identity.Binary, want.Identity.Binary)
	}
	if got.Marker() == "" || got.Marker() != want.Marker() || got.MarkerSHA256() == "" || got.MarkerSHA256() != want.MarkerSHA256() {
		t.Fatalf("marker got %q / %s", got.Marker(), got.MarkerSHA256())
	}
	if got.Identity.Image != (domain.ProbeImageSelection{}) || got != want {
		t.Fatalf("intent got %#v want %#v", got, want)
	}
}

func assertStageHeaderRefused(t *testing.T, got domain.ProbeBinaryStageIntent, err error) {
	t.Helper()
	if got != (domain.ProbeBinaryStageIntent{}) || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatalf("got %#v err %v", got, err)
	}
}

func mustStageHeaderEncode(t *testing.T, intent domain.ProbeBinaryStageIntent) []byte {
	t.Helper()
	out, err := EncodeProbeBinaryStageHeader(intent)
	if err != nil || out == nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

func stageHeaderFrame(t *testing.T, payload []byte) []byte {
	t.Helper()
	if len(payload) < 1 || len(payload) > MaxProbeBinaryStageHeaderSize {
		t.Fatalf("payload length %d", len(payload))
	}
	out := make([]byte, ProbeBinaryStagePrefixSize+len(payload))
	copy(out, "PHXSTG1\n")
	binary.BigEndian.PutUint32(out[8:12], uint32(len(payload)))
	copy(out[12:], payload)
	return out
}

func stageHeaderPrefix(magic string, n uint32) []byte {
	out := make([]byte, ProbeBinaryStagePrefixSize)
	copy(out, magic)
	binary.BigEndian.PutUint32(out[8:12], n)
	return out
}

func stageHeaderFixture(t *testing.T, max bool, arch string) domain.ProbeBinaryStageIntent {
	t.Helper()
	intent := domain.ProbeBinaryStageIntent{
		Identity: domain.ProbeInstallIdentity{
			OperationID:          "11111111-1111-4111-8111-111111111111",
			Host:                 domain.ProbeSSHHost{Address: "127.0.0.1:22", User: "root"},
			HostKeyFingerprint:   stageHeaderFingerprint(0),
			ClientKeyFingerprint: stageHeaderFingerprint(0),
			RequestedBy:          7,
			LayoutVersion:        domain.ProbeInstallLayoutVersion,
			Mode:                 domain.ProbeInstallSystemd,
			Binary: domain.ProbeBinaryPin{
				Version:      "1.2.3",
				Architecture: arch,
				SHA256:       strings.Repeat("0", 64),
				Size:         9,
			},
		},
		OwnerID:   "22222222-2222-4222-8222-222222222222",
		AttemptID: "33333333-3333-4333-8333-333333333333",
	}
	if max {
		intent.Identity.OperationID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
		intent.OwnerID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
		intent.AttemptID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		intent.Identity.Host.User = strings.Repeat("u", 64)
		intent.Identity.Host.Address = strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61) + ":65535"
		intent.Identity.RequestedBy = math.MaxInt64
		intent.Identity.Binary.Version = "1.2.3-" + strings.Repeat("a", 58)
		intent.Identity.Binary.SHA256 = strings.Repeat("ab", 32)
		intent.Identity.Binary.Size = domain.MaxProbeBinarySize
	}
	if !intent.Valid() {
		t.Fatalf("fixture invalid max=%v arch=%s", max, arch)
	}
	return intent
}

func stageHeaderFingerprint(b byte) string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

func testStageHeaderJSON(in domain.ProbeBinaryStageIntent) string {
	id, pin := in.Identity, in.Identity.Binary
	return `{"operation_id":` + strconv.Quote(id.OperationID) +
		`,"owner_id":` + strconv.Quote(in.OwnerID) +
		`,"attempt_id":` + strconv.Quote(in.AttemptID) +
		`,"ssh_address":` + strconv.Quote(id.Host.Address) +
		`,"ssh_user":` + strconv.Quote(id.Host.User) +
		`,"host_key_fingerprint":` + strconv.Quote(id.HostKeyFingerprint) +
		`,"client_key_fingerprint":` + strconv.Quote(id.ClientKeyFingerprint) +
		`,"requested_by":` + strconv.FormatInt(id.RequestedBy, 10) +
		`,"layout_version":` + strconv.Itoa(id.LayoutVersion) +
		`,"mode":` + strconv.Quote(id.Mode) +
		`,"version":` + strconv.Quote(pin.Version) +
		`,"architecture":` + strconv.Quote(pin.Architecture) +
		`,"sha256":` + strconv.Quote(pin.SHA256) +
		`,"size":` + strconv.FormatInt(pin.Size, 10) + `}`
}

const stageHeaderLiteral = `{"operation_id":"11111111-1111-4111-8111-111111111111","owner_id":"22222222-2222-4222-8222-222222222222","attempt_id":"33333333-3333-4333-8333-333333333333","ssh_address":"127.0.0.1:22","ssh_user":"root","host_key_fingerprint":"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_key_fingerprint":"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","requested_by":7,"layout_version":1,"mode":"systemd","version":"1.2.3","architecture":"amd64","sha256":"0000000000000000000000000000000000000000000000000000000000000000","size":9}`
