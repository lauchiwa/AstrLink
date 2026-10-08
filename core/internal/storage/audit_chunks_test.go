package storage

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"slices"
	"testing"
)

func auditChunkTestData(seed uint64, size int) []byte {
	random := rand.New(rand.NewPCG(seed, seed^0x5bd1e995))
	data := make([]byte, size)
	for index := range data {
		data[index] = byte(random.Uint32())
	}
	return data
}

func auditChunkCuts(chunks [][]byte) []int {
	cuts := make([]int, 0, len(chunks))
	offset := 0
	for _, chunk := range chunks {
		offset += len(chunk)
		cuts = append(cuts, offset)
	}
	return cuts
}

// Stored chunks are matched by content across builds, so the boundaries for
// a given input must never move by accident.
func TestSplitAuditChunksIsStable(t *testing.T) {
	data := auditChunkTestData(1, 96<<10)
	chunks := SplitAuditChunks(data)
	if !bytes.Equal(bytes.Join(chunks, nil), data) {
		t.Fatal("chunks do not rebuild the input")
	}
	for index, chunk := range chunks {
		last := index == len(chunks)-1
		if len(chunk) > auditChunkMaxBytes || (!last && len(chunk) < auditChunkMinBytes) {
			t.Fatalf("chunk %d has %d bytes", index, len(chunk))
		}
	}
	want := []int{10308, 19891, 28801, 30998, 40043, 42241, 52137, 67698, 75940, 84394, 98304}
	if got := auditChunkCuts(chunks); !slices.Equal(got, want) {
		t.Fatalf("cuts = %v\nwant %v", got, want)
	}
	if len(SplitAuditChunks(nil)) != 0 {
		t.Fatal("empty input produced chunks")
	}
}

func TestSplitAuditChunksReusesAHistoryThatOnlyGrew(t *testing.T) {
	history := auditChunkTestData(2, 300<<10)
	grown := append(slices.Clone(history), auditChunkTestData(3, 30<<10)...)
	before, after := SplitAuditChunks(history), SplitAuditChunks(grown)
	for index := range len(before) - 1 {
		if !bytes.Equal(before[index], after[index]) {
			t.Fatalf("chunk %d changed when the history only grew", index)
		}
	}
}

func TestSplitAuditChunksRealignsAfterAnEdit(t *testing.T) {
	data := auditChunkTestData(4, 512<<10)
	edited := slices.Clone(data)
	edited[200_000] ^= 0xff
	known := map[string]bool{}
	for _, chunk := range SplitAuditChunks(data) {
		known[string(chunk)] = true
	}
	changed := 0
	for _, chunk := range SplitAuditChunks(edited) {
		if !known[string(chunk)] {
			changed++
		}
	}
	if changed > 2 {
		t.Fatalf("one edited byte changed %d chunks", changed)
	}
}

func TestAuditRecipeRoundTripAndRejectsMalformedRecipes(t *testing.T) {
	key := bytes.Repeat([]byte{7}, AuditKeyBytes)
	seal := func(plain string) AuditChunk {
		nonce, ciphertext, err := SealAuditBlob(key, []byte(plain))
		if err != nil {
			t.Fatal(err)
		}
		return AuditChunk{Nonce: nonce, Ciphertext: ciphertext}
	}
	chunks := []AuditChunk{seal("shared one "), seal("shared two")}
	recipe := EncodeAuditRecipe([]AuditRecipePiece{
		{Literal: []byte("secret ")}, {}, {Literal: []byte{}}, {}, {Literal: []byte(" tail")},
	})
	got, err := AssembleAuditRecipe(recipe, key, chunks)
	if err != nil || string(got) != "secret shared one shared two tail" {
		t.Fatalf("assembled = %q, %v", got, err)
	}
	opened, err := OpenAuditChunks(key, chunks)
	if err != nil || string(opened) != "shared one shared two" {
		t.Fatalf("opened = %q, %v", opened, err)
	}
	for name, malformed := range map[string]struct {
		recipe []byte
		chunks []AuditChunk
	}{
		"unknown version": {append([]byte{9}, recipe[1:]...), chunks},
		"missing chunk":   {recipe, chunks[:1]},
		"unused chunk":    {recipe, append(slices.Clone(chunks), seal("extra"))},
		"literal overrun": {[]byte{auditRecipeVersion, auditRecipeLiteral, 50, 'x'}, nil},
		"unknown tag":     {[]byte{auditRecipeVersion, 7}, nil},
	} {
		if _, err := AssembleAuditRecipe(malformed.recipe, key, malformed.chunks); !errors.Is(err, ErrAuditDecrypt) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	other := bytes.Repeat([]byte{8}, AuditKeyBytes)
	if _, err := AssembleAuditRecipe(recipe, other, chunks); !errors.Is(err, ErrAuditDecrypt) {
		t.Fatalf("wrong key: err = %v", err)
	}
}
