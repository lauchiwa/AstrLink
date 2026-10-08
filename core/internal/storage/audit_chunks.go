package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// AuditLayout is how a stored part's bytes are laid out.
type AuditLayout string

const (
	// AuditLayoutWhole parts hold their bytes in one sealed copy.
	AuditLayoutWhole AuditLayout = "whole"
	// AuditLayoutChunks parts are their chunks, in order, each sealed under
	// dek_audit and shared within the session. Only shareable parts use it.
	AuditLayoutChunks AuditLayout = "chunks"
	// AuditLayoutRecipe parts are raw_v1 parts whose sealed bytes are a
	// recipe: literal runs, and places where a shareable chunk of the same
	// session goes. Only what equals shareable content is referenced.
	AuditLayoutRecipe AuditLayout = "recipe"
)

func (layout AuditLayout) Valid() bool {
	switch layout {
	case AuditLayoutWhole, AuditLayoutChunks, AuditLayoutRecipe:
		return true
	default:
		return false
	}
}

// AuditChunk is one stored chunk, sealed under dek_audit.
type AuditChunk struct {
	Nonce      []byte
	Ciphertext []byte
}

// Content-defined chunk bounds. Agent requests resend their history, so a
// boundary must depend on nearby bytes only: the same history then cuts into
// the same chunks however much was appended. Changing these or the gear
// table only stops new chunks matching old ones; nothing breaks.
const (
	auditChunkMinBytes = 2 << 10
	auditChunkAvgBytes = 8 << 10
	auditChunkMaxBytes = 64 << 10
	// Below the average a boundary is harder to hit, above it easier
	// (FastCDC normalisation), which keeps chunk sizes near the average.
	auditChunkMaskSmall = uint64(1<<15-1) << (64 - 15)
	auditChunkMaskLarge = uint64(1<<11-1) << (64 - 11)
)

var auditChunkGear = func() (table [256]uint64) {
	// SplitMix64 from a fixed seed: arbitrary, but the same in every build.
	state := uint64(0x6a09e667f3bcc909)
	for index := range table {
		state += 0x9e3779b97f4a7c15
		value := state
		value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
		value = (value ^ (value >> 27)) * 0x94d049bb133111eb
		table[index] = value ^ (value >> 31)
	}
	return table
}()

// SplitAuditChunks cuts data into content-defined chunks that alias data.
func SplitAuditChunks(data []byte) [][]byte {
	chunks := make([][]byte, 0, len(data)/auditChunkAvgBytes+1)
	for len(data) > 0 {
		cut := auditChunkCut(data)
		chunks = append(chunks, data[:cut:cut])
		data = data[cut:]
	}
	return chunks
}

func auditChunkCut(data []byte) int {
	size := len(data)
	if size <= auditChunkMinBytes {
		return size
	}
	size = min(size, auditChunkMaxBytes)
	normal := min(size, auditChunkAvgBytes)
	var hash uint64
	index := auditChunkMinBytes
	for ; index < normal; index++ {
		hash = hash<<1 + auditChunkGear[data[index]]
		if hash&auditChunkMaskSmall == 0 {
			return index + 1
		}
	}
	for ; index < size; index++ {
		hash = hash<<1 + auditChunkGear[data[index]]
		if hash&auditChunkMaskLarge == 0 {
			return index + 1
		}
	}
	return size
}

// AuditChunkKey names a chunk within its scope (the session) without
// revealing it. Only shareable content, which dek_audit already opens, is
// ever stored under such a key, so the key adds nothing for its holder.
func AuditChunkKey(auditKey []byte, scope string, chunk []byte) []byte {
	digest := hmac.New(sha256.New, auditKey)
	_, _ = digest.Write([]byte("audit-chunk"))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(scope))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(chunk)
	return digest.Sum(nil)
}

// OpenAuditChunks decrypts the chunks of a chunks-layout part, in order.
func OpenAuditChunks(auditKey []byte, chunks []AuditChunk) ([]byte, error) {
	var assembled []byte
	for _, chunk := range chunks {
		plain, err := OpenAuditBlob(auditKey, chunk.Nonce, chunk.Ciphertext)
		if err != nil {
			clear(assembled)
			return nil, err
		}
		assembled = append(assembled, plain...)
		clear(plain)
	}
	return assembled, nil
}

const (
	auditRecipeVersion = 1
	auditRecipeLiteral = 0
	auditRecipeChunk   = 1
)

var errAuditRecipe = errors.New("malformed audit recipe")

// AuditRecipePiece is one run of a recipe: literal bytes, or the next
// referenced chunk when Literal is nil.
type AuditRecipePiece struct {
	Literal []byte
}

// EncodeAuditRecipe writes pieces as a recipe. Chunk pieces take the part's
// chunk references in order, so the recipe never names a chunk itself.
func EncodeAuditRecipe(pieces []AuditRecipePiece) []byte {
	encoded := []byte{auditRecipeVersion}
	for _, piece := range pieces {
		if piece.Literal == nil {
			encoded = append(encoded, auditRecipeChunk)
			continue
		}
		encoded = append(encoded, auditRecipeLiteral)
		encoded = binary.AppendUvarint(encoded, uint64(len(piece.Literal)))
		encoded = append(encoded, piece.Literal...)
	}
	return encoded
}

// AssembleAuditRecipe rebuilds a recipe-layout part from its opened recipe
// and its referenced chunks, which open with dek_audit.
func AssembleAuditRecipe(recipe []byte, auditKey []byte, chunks []AuditChunk) ([]byte, error) {
	if len(recipe) == 0 || recipe[0] != auditRecipeVersion {
		return nil, fmt.Errorf("%w: %w: version", ErrAuditDecrypt, errAuditRecipe)
	}
	var assembled []byte
	fail := func(reason string) ([]byte, error) {
		clear(assembled)
		return nil, fmt.Errorf("%w: %w: %s", ErrAuditDecrypt, errAuditRecipe, reason)
	}
	next := 0
	for rest := recipe[1:]; len(rest) > 0; {
		tag := rest[0]
		rest = rest[1:]
		switch tag {
		case auditRecipeChunk:
			if next >= len(chunks) {
				return fail("missing chunk")
			}
			plain, err := OpenAuditBlob(auditKey, chunks[next].Nonce, chunks[next].Ciphertext)
			if err != nil {
				clear(assembled)
				return nil, err
			}
			assembled = append(assembled, plain...)
			clear(plain)
			next++
		case auditRecipeLiteral:
			size, read := binary.Uvarint(rest)
			if read <= 0 || size > uint64(len(rest)-read) {
				return fail("literal length")
			}
			rest = rest[read:]
			assembled = append(assembled, rest[:size]...)
			rest = rest[size:]
		default:
			return fail("tag")
		}
	}
	if next != len(chunks) {
		return fail("unused chunk")
	}
	return assembled, nil
}
