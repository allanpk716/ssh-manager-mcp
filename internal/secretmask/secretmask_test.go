package secretmask

import (
	"bytes"
	"math/rand"
	"sort"
	"testing"
)

func TestMaskReplacesCurrentAndPreviousValues(t *testing.T) {
	masker := New([]Secret{{
		Name:              "database-password",
		Current:           []byte("current-secret"),
		Previous:          []byte("previous-secret"),
		WritableServerIDs: []string{"server-01"},
	}})

	got := masker.Mask([]byte("current-secret previous-secret plain"), "")

	want := []byte("{{sshmgr-secret:database-password}} {{sshmgr-secret:database-password}} plain")
	if !bytes.Equal(got.Output, want) {
		t.Fatalf("output = %q, want %q", got.Output, want)
	}
	if len(got.MaskedNames) != 1 || got.MaskedNames[0] != "database-password" {
		t.Fatalf("masked names = %#v, want [database-password]", got.MaskedNames)
	}
}

func TestMaskPrefersLongerValueAtSamePosition(t *testing.T) {
	masker := New([]Secret{
		{Name: "short-token", Current: []byte("token")},
		{Name: "database-url", Current: []byte("token-for-database")},
	})

	got := masker.Mask([]byte("token-for-database token"), "")

	want := []byte("{{sshmgr-secret:database-url}} {{sshmgr-secret:short-token}}")
	if !bytes.Equal(got.Output, want) {
		t.Fatalf("output = %q, want %q", got.Output, want)
	}
	wantNames := []string{"database-url", "short-token"}
	if !equalStrings(got.MaskedNames, wantNames) {
		t.Fatalf("masked names = %#v, want %#v", got.MaskedNames, wantNames)
	}
}

func TestMaskChoosesNameForIdenticalValue(t *testing.T) {
	masker := New([]Secret{
		{Name: "zabbix-token", Current: []byte("shared-secret"), WritableServerIDs: []string{"server-01"}},
		{Name: "api-token", Current: []byte("shared-secret")},
	})

	withoutCurrentServer := masker.Mask([]byte("shared-secret"), "")
	if !equalStrings(withoutCurrentServer.MaskedNames, []string{"api-token"}) {
		t.Fatalf("without current server: masked names = %#v, want [api-token]", withoutCurrentServer.MaskedNames)
	}

	withCurrentServer := masker.Mask([]byte("shared-secret"), "server-01")
	if !equalStrings(withCurrentServer.MaskedNames, []string{"zabbix-token"}) {
		t.Fatalf("with current server: masked names = %#v, want [zabbix-token]", withCurrentServer.MaskedNames)
	}
}

func TestMaskMatchesBothNewlineWritings(t *testing.T) {
	masker := New([]Secret{{
		Name:    "deployment-key",
		Current: []byte("first\r\nsecond\nthird"),
	}})

	carriageReturnAndLineFeed := masker.Mask([]byte("first\r\nsecond\nthird"), "")
	if !bytes.Equal(carriageReturnAndLineFeed.Output, []byte("{{sshmgr-secret:deployment-key}}")) {
		t.Fatalf("carriage return and line feed output = %q", carriageReturnAndLineFeed.Output)
	}

	lineFeedOnly := masker.Mask([]byte("first\nsecond\nthird"), "")
	if !bytes.Equal(lineFeedOnly.Output, []byte("{{sshmgr-secret:deployment-key}}")) {
		t.Fatalf("line feed only output = %q", lineFeedOnly.Output)
	}

	carriageReturnAndLineFeedEverywhere := masker.Mask([]byte("first\r\nsecond\r\nthird"), "")
	if !bytes.Equal(carriageReturnAndLineFeedEverywhere.Output, []byte("{{sshmgr-secret:deployment-key}}")) {
		t.Fatalf("carriage return and line feed everywhere output = %q", carriageReturnAndLineFeedEverywhere.Output)
	}
	if masker.LongestMatchLength() != len("first\r\nsecond\r\nthird") {
		t.Fatalf("longest match length = %d, want %d", masker.LongestMatchLength(), len("first\r\nsecond\r\nthird"))
	}
}

func TestMaskHandlesRawBytesAdjacentAndOverlappingMatches(t *testing.T) {
	rawValue := []byte{0xff, 0x00, 0xfe}
	overlappingValue := []byte{0x00, 0xfe, 0xfd}
	masker := New([]Secret{
		{Name: "raw-token", Current: rawValue},
		{Name: "overlapping-token", Current: overlappingValue},
		{Name: "adjacent-token", Current: []byte("xy")},
	})

	rawInput := append([]byte{}, rawValue...)
	rawInput = append(rawInput, 0xfd)
	rawInput = append(rawInput, rawValue...)
	got := masker.Mask(rawInput, "")

	want := []byte("{{sshmgr-secret:raw-token}}\xfd{{sshmgr-secret:raw-token}}")
	if !bytes.Equal(got.Output, want) {
		t.Fatalf("raw and overlapping output = %x, want %x", got.Output, want)
	}

	adjacent := masker.Mask([]byte("xyxy"), "")
	wantAdjacent := []byte("{{sshmgr-secret:adjacent-token}}{{sshmgr-secret:adjacent-token}}")
	if !bytes.Equal(adjacent.Output, wantAdjacent) {
		t.Fatalf("adjacent output = %q, want %q", adjacent.Output, wantAdjacent)
	}
}

func TestMaskWithEmptyCollectionReturnsInputWithoutAllocation(t *testing.T) {
	masker := New(nil)
	input := []byte("ordinary command output")

	got := masker.Mask(input, "server-01")
	if !bytes.Equal(got.Output, input) || got.MaskedNames != nil {
		t.Fatalf("result = %#v, want original input and no masked names", got)
	}
	if masker.LongestMatchLength() != 0 {
		t.Fatalf("longest match length = %d, want 0", masker.LongestMatchLength())
	}

	var emptyCollectionMasker Masker
	allocations := testing.AllocsPerRun(100, func() {
		emptyCollectionMasker = New(nil)
		got = emptyCollectionMasker.Mask(input, "server-01")
	})
	if allocations != 0 {
		t.Fatalf("allocations = %f, want 0", allocations)
	}
}

func TestStreamKeepsTailUntilItCanBeDecidedAndFlushesAtEnd(t *testing.T) {
	masker := New([]Secret{{Name: "token", Current: []byte("abcdef")}})

	incomplete := masker.NewStream("")
	first := incomplete.Write([]byte("prefixabc"))
	if !bytes.Equal(first.Output, []byte("pref")) || first.MaskedNames != nil {
		t.Fatalf("first write result = %#v, want pref", first)
	}
	second := incomplete.Write([]byte("def"))
	wantSecond := []byte("ix{{sshmgr-secret:token}}")
	if !bytes.Equal(second.Output, wantSecond) || !equalStrings(second.MaskedNames, []string{"token"}) {
		t.Fatalf("second write result = %#v, want %q with token", second, wantSecond)
	}
	flushed := incomplete.Flush()
	if len(flushed.Output) != 0 || flushed.MaskedNames != nil {
		t.Fatalf("flush result = %#v, want empty output", flushed)
	}

	complete := masker.NewStream("")
	decided := complete.Write([]byte("prefixabcdefsuffix"))
	wantDecided := []byte("prefix{{sshmgr-secret:token}}s")
	if !bytes.Equal(decided.Output, wantDecided) {
		t.Fatalf("decided output = %q, want %q", decided.Output, wantDecided)
	}
	if !equalStrings(decided.MaskedNames, []string{"token"}) {
		t.Fatalf("decided masked names = %#v, want [token]", decided.MaskedNames)
	}
	remaining := complete.Flush()
	wantRemaining := []byte("uffix")
	if !bytes.Equal(remaining.Output, wantRemaining) || remaining.MaskedNames != nil {
		t.Fatalf("remaining output = %#v, want %q without masked names", remaining, wantRemaining)
	}
}

func TestStreamWithEmptyCollectionReturnsEachWriteWithoutAllocation(t *testing.T) {
	masker := New(nil)
	input := []byte("streaming command output")

	var stream Stream
	var written MaskResult
	var flushed MaskResult
	allocations := testing.AllocsPerRun(100, func() {
		stream = masker.NewStream("server-01")
		written = stream.Write(input)
		flushed = stream.Flush()
	})
	if allocations != 0 {
		t.Fatalf("allocations = %f, want 0", allocations)
	}
	if !bytes.Equal(written.Output, input) || written.MaskedNames != nil {
		t.Fatalf("write result = %#v, want original input and no masked names", written)
	}
	if len(flushed.Output) != 0 || flushed.MaskedNames != nil {
		t.Fatalf("flush result = %#v, want empty output", flushed)
	}
}

func TestStreamMatchesWholeMaskForRandomAndByteByByteSplits(t *testing.T) {
	masker := New([]Secret{
		{Name: "alpha-token", Current: []byte("alpha-secret-1234"), Previous: []byte("old-alpha-secret")},
		{Name: "beta-token", Current: []byte("beta-secret")},
		{Name: "multi-line-token", Current: []byte("first\r\nsecond")},
	})
	input := []byte("start alpha-secret-1234 middle beta-secret old-alpha-secret first\nsecond end")
	whole := masker.Mask(input, "")

	random := rand.New(rand.NewSource(117))
	for iteration := 0; iteration < 100; iteration++ {
		var chunks [][]byte
		for offset := 0; offset < len(input); {
			chunkLength := 1 + random.Intn(len(input)-offset)
			chunks = append(chunks, input[offset:offset+chunkLength])
			offset += chunkLength
		}
		got := maskByChunks(masker, chunks)
		if !bytes.Equal(got.Output, whole.Output) {
			t.Fatalf("random split %d: output = %q, want %q", iteration, got.Output, whole.Output)
		}
		if !equalStrings(got.MaskedNames, whole.MaskedNames) {
			t.Fatalf("random split %d: masked names = %#v, want %#v", iteration, got.MaskedNames, whole.MaskedNames)
		}
	}

	var byteChunks [][]byte
	for _, oneByte := range input {
		byteChunks = append(byteChunks, []byte{oneByte})
	}
	got := maskByChunks(masker, byteChunks)
	if !bytes.Equal(got.Output, whole.Output) {
		t.Fatalf("byte-by-byte output = %q, want %q", got.Output, whole.Output)
	}
	if !equalStrings(got.MaskedNames, whole.MaskedNames) {
		t.Fatalf("byte-by-byte masked names = %#v, want %#v", got.MaskedNames, whole.MaskedNames)
	}
}

func maskByChunks(masker Masker, chunks [][]byte) MaskResult {
	stream := masker.NewStream("")
	var output []byte
	var names []string
	for _, chunk := range chunks {
		result := stream.Write(chunk)
		output = append(output, result.Output...)
		names = append(names, result.MaskedNames...)
	}
	result := stream.Flush()
	output = append(output, result.Output...)
	names = append(names, result.MaskedNames...)
	sort.Strings(names)
	return MaskResult{Output: output, MaskedNames: deduplicateStrings(names)}
}

func equalStrings(got []string, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
