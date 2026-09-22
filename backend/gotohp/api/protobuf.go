// Package api implements the unofficial Google Photos "native" (Android
// app) upload protocol against photos.googleapis.com / photosdata-pa.googleapis.com.
//
// This is a from-scratch reimplementation based on the protocol documented
// by https://github.com/xob0t/gotohp (MIT licensed) — specifically its
// .proto field definitions at https://github.com/xob0t/gotohp/tree/main/.proto.
// We do not import gotohp's generated code (its module isn't go-gettable
// and its API is built around global mutable state); instead we hand-encode
// the small subset of fields gotohp's own code actually populates. Most of
// the upstream .proto schema (particularly CommitUpload/CommitUploadResponse)
// is unpopulated placeholder structure left over from generic protobuf
// reverse-engineering and is intentionally not reproduced here.
package api

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

// pbBuilder is a minimal append-only protobuf wire encoder covering just
// the varint, bytes/string, and nested-message field types used by the
// messages below.
type pbBuilder []byte

func (b pbBuilder) varint(num protowire.Number, v uint64) pbBuilder {
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func (b pbBuilder) bytes(num protowire.Number, v []byte) pbBuilder {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func (b pbBuilder) str(num protowire.Number, v string) pbBuilder {
	return b.bytes(num, []byte(v))
}

// message appends nested as a length-delimited embedded message.
func (b pbBuilder) message(num protowire.Number, nested pbBuilder) pbBuilder {
	return b.bytes(num, nested)
}

// pbValue is one decoded occurrence of a field.
type pbValue struct {
	varint uint64
	bytes  []byte
}

// pbFields is a parsed protobuf message: field number -> occurrences in
// wire order. Reading methods return the last occurrence, matching proto3
// "last one wins" semantics for non-repeated fields.
type pbFields map[protowire.Number][]pbValue

func parsePBFields(b []byte) (pbFields, error) {
	fields := pbFields{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, fmt.Errorf("gotohp: invalid protobuf tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, fmt.Errorf("gotohp: invalid protobuf varint: %w", protowire.ParseError(n))
			}
			fields[num] = append(fields[num], pbValue{varint: v})
			b = b[n:]
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, fmt.Errorf("gotohp: invalid protobuf bytes: %w", protowire.ParseError(n))
			}
			fields[num] = append(fields[num], pbValue{bytes: v})
			b = b[n:]
		case protowire.Fixed32Type:
			v, n := protowire.ConsumeFixed32(b)
			if n < 0 {
				return nil, fmt.Errorf("gotohp: invalid protobuf fixed32: %w", protowire.ParseError(n))
			}
			fields[num] = append(fields[num], pbValue{varint: uint64(v)})
			b = b[n:]
		case protowire.Fixed64Type:
			v, n := protowire.ConsumeFixed64(b)
			if n < 0 {
				return nil, fmt.Errorf("gotohp: invalid protobuf fixed64: %w", protowire.ParseError(n))
			}
			fields[num] = append(fields[num], pbValue{varint: v})
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return nil, fmt.Errorf("gotohp: invalid protobuf field: %w", protowire.ParseError(n))
			}
			b = b[n:]
		}
	}
	return fields, nil
}

func (f pbFields) bytesAt(num protowire.Number) []byte {
	if vs, ok := f[num]; ok && len(vs) > 0 {
		return vs[len(vs)-1].bytes
	}
	return nil
}

func (f pbFields) strAt(num protowire.Number) string {
	return string(f.bytesAt(num))
}

func (f pbFields) varintAt(num protowire.Number) uint64 {
	if vs, ok := f[num]; ok && len(vs) > 0 {
		return vs[len(vs)-1].varint
	}
	return 0
}

// msg parses field num as a nested message. Safe to chain on a nil/missing
// result since pbFields is a map type and reads on a nil map are no-ops.
func (f pbFields) msg(num protowire.Number) pbFields {
	b := f.bytesAt(num)
	if b == nil {
		return nil
	}
	nested, err := parsePBFields(b)
	if err != nil {
		return nil
	}
	return nested
}

// --- GetUploadToken (.proto/GetUploadToken.proto) ---
// int32 f1=1, f2=2, f3=3, f4=4; int64 file_size_bytes=7.
// gotohp always sends the constant f1=2,f2=2,f3=1,f4=3.
func EncodeGetUploadToken(fileSize int64) []byte {
	var b pbBuilder
	b = b.varint(1, 2)
	b = b.varint(2, 2)
	b = b.varint(3, 1)
	b = b.varint(4, 3)
	b = b.varint(7, uint64(fileSize))
	return b
}

// --- HashCheck (.proto/HashCheck.proto) ---
// field1{ field1{ bytes sha1Hash=1 }=1, field2{}=2 }
func EncodeHashCheck(sha1Hash []byte) []byte {
	var inner pbBuilder
	inner = inner.bytes(1, sha1Hash)
	var f1 pbBuilder
	f1 = f1.message(1, inner)
	f1 = f1.message(2, nil)
	var b pbBuilder
	b = b.message(1, f1)
	return b
}

// --- RemoteMatches (.proto/RemoteMatches.proto) ---
// media key lives at field1.field2.field2.media_key (string, tag 1).
func DecodeRemoteMatchesMediaKey(data []byte) (string, error) {
	f, err := parsePBFields(data)
	if err != nil {
		return "", err
	}
	return f.msg(1).msg(2).msg(2).strAt(1), nil
}

// --- CommitToken (.proto/CommitToken.proto) ---
// int64 field1=1; bytes field2=2. Returned as the raw body of the
// interactive-upload PUT response, to be echoed back into CommitUpload.
type CommitToken struct {
	Field1 uint64
	Field2 []byte
}

func DecodeCommitToken(data []byte) (CommitToken, error) {
	f, err := parsePBFields(data)
	if err != nil {
		return CommitToken{}, err
	}
	return CommitToken{Field1: f.varintAt(1), Field2: f.bytesAt(2)}, nil
}

// --- CommitUpload (.proto/CommitUpload.proto) ---
// Only the leaf fields gotohp's own code sets are encoded; the rest of the
// upstream schema's enormous nested-empty-message tree is unused dead
// structure and intentionally omitted:
//
//	field1 (msg) {
//	  field1 (msg) { field1=CommitToken.Field1, field2=CommitToken.Field2 }
//	  file_name=2 (string), sha1_hash=3 (bytes)
//	  field4 (msg) { file_last_modified_timestamp=1, field2=2 (constant 46000000) }
//	  quality=7, field10=10 (constant 1)
//	}
//	field2 (msg) { model=3, make=4, android_api_version=5 }
//	field3 = bytes{1,3}
func EncodeCommitUpload(token CommitToken, fileName string, sha1Hash []byte, uploadTimestamp int64, quality int64, model, deviceMake string, androidAPIVersion int64) []byte {
	var tokenMsg pbBuilder
	tokenMsg = tokenMsg.varint(1, token.Field1)
	tokenMsg = tokenMsg.bytes(2, token.Field2)

	var mtimeMsg pbBuilder
	mtimeMsg = mtimeMsg.varint(1, uint64(uploadTimestamp))
	mtimeMsg = mtimeMsg.varint(2, 46000000)

	var f1 pbBuilder
	f1 = f1.message(1, tokenMsg)
	f1 = f1.str(2, fileName)
	f1 = f1.bytes(3, sha1Hash)
	f1 = f1.message(4, mtimeMsg)
	f1 = f1.varint(7, uint64(quality))
	f1 = f1.varint(10, 1)

	var device pbBuilder
	device = device.str(3, model)
	device = device.str(4, deviceMake)
	device = device.varint(5, uint64(androidAPIVersion))

	var b pbBuilder
	b = b.message(1, f1)
	b = b.message(2, device)
	b = b.bytes(3, []byte{1, 3})
	return b
}

// --- CommitUploadResponse (.proto/CommitUploadResponse.proto) ---
// media key lives at field1.field3.media_key (string, tag 1).
func DecodeCommitUploadResponseMediaKey(data []byte) (string, error) {
	f, err := parsePBFields(data)
	if err != nil {
		return "", err
	}
	mk := f.msg(1).msg(3).strAt(1)
	if mk == "" {
		return "", fmt.Errorf("gotohp: commit response missing media key")
	}
	return mk, nil
}

// --- CreateAlbum (.proto/CreateAlbum.proto) ---
//
//	album_name=1, timestamp=2, field3=3 (constant 1)
//	media_keys=4 (repeated msg { field1 (msg) { media_key=1 } })
//	field6=6 (empty msg), field7=7 (msg { field1=1 (constant 3) })
//	device_info=8 (msg { model=3, make=4, android_api_version=5 })
func EncodeCreateAlbum(albumName string, mediaKeys []string, timestamp int64, model, deviceMake string, androidAPIVersion int64) []byte {
	var b pbBuilder
	b = b.str(1, albumName)
	b = b.varint(2, uint64(timestamp))
	b = b.varint(3, 1)
	for _, mk := range mediaKeys {
		var inner pbBuilder
		inner = inner.str(1, mk)
		var entry pbBuilder
		entry = entry.message(1, inner)
		b = b.message(4, entry)
	}
	b = b.message(6, nil)
	var f7 pbBuilder
	f7 = f7.varint(1, 3)
	b = b.message(7, f7)
	var device pbBuilder
	device = device.str(3, model)
	device = device.str(4, deviceMake)
	device = device.varint(5, uint64(androidAPIVersion))
	b = b.message(8, device)
	return b
}

// --- CreateAlbumResponse (.proto/CreateAlbumResponse.proto) ---
// album media key lives at field1.album_media_key (string, tag 1).
func DecodeCreateAlbumResponseAlbumMediaKey(data []byte) (string, error) {
	f, err := parsePBFields(data)
	if err != nil {
		return "", err
	}
	amk := f.msg(1).strAt(1)
	if amk == "" {
		return "", fmt.Errorf("gotohp: create album response missing album media key")
	}
	return amk, nil
}

// --- AddMediaToAlbum (.proto/AddMediaToAlbum.proto) ---
//
//	media_keys=1 (repeated string), album_media_key=2
//	field5=5 (msg { field1=1 (constant 2) })
//	device_info=6 (msg { model=3, make=4, android_api_version=5 })
//	timestamp=7
func EncodeAddMediaToAlbum(mediaKeys []string, albumMediaKey string, timestamp int64, model, deviceMake string, androidAPIVersion int64) []byte {
	var b pbBuilder
	for _, mk := range mediaKeys {
		b = b.str(1, mk)
	}
	b = b.str(2, albumMediaKey)
	var f5 pbBuilder
	f5 = f5.varint(1, 2)
	b = b.message(5, f5)
	var device pbBuilder
	device = device.str(3, model)
	device = device.str(4, deviceMake)
	device = device.varint(5, uint64(androidAPIVersion))
	b = b.message(6, device)
	b = b.varint(7, uint64(timestamp))
	return b
}

// --- Library Listing (get_library_state / get_library_page) ---
// Endpoint: photosdata-pa.googleapis.com/6439526531001121323/18047484249733410717
//
// The request contains a massive "field mask" telling the server what data to
// include. This builds a minimal mask sufficient for file listing (name, size,
// hash, timestamps). The server is tolerant of minimal masks.

// libraryStateTemplate is the full protobuf request for get_library_state
// with an empty sync_token. Captured from gpmc's get_library_state format.
// The request contains a massive field mask telling the server what data to
// return. This template is used as-is for initial sync (no sync_token), or
// with the sync_token field (1.6) appended for delta sync.
//
//nolint:unused // embedded binary template
var libraryStateTemplate = []byte{
	0x0a, 0xcd, 0x05, 0x0a, 0xab, 0x02, 0x0a, 0x59, 0x0a, 0x00, 0x1a, 0x00, 0x22, 0x00, 0x2a, 0x0c,
	0x0a, 0x00, 0x12, 0x00, 0x1a, 0x00, 0x22, 0x00, 0x2a, 0x00, 0x3a, 0x00, 0x32, 0x00, 0x3a, 0x02,
	0x12, 0x00, 0x7a, 0x00, 0x82, 0x01, 0x00, 0x8a, 0x01, 0x00, 0x9a, 0x01, 0x00, 0xa2, 0x01, 0x00,
	0xaa, 0x01, 0x06, 0x2a, 0x02, 0x1a, 0x00, 0x32, 0x00, 0xca, 0x01, 0x00, 0xf2, 0x01, 0x02, 0x12,
	0x00, 0xfa, 0x01, 0x00, 0x82, 0x02, 0x00, 0x8a, 0x02, 0x02, 0x0a, 0x00, 0x92, 0x02, 0x00, 0xa2,
	0x02, 0x00, 0xaa, 0x02, 0x00, 0xb2, 0x02, 0x00, 0xba, 0x02, 0x00, 0xc2, 0x02, 0x00, 0xca, 0x02,
	0x00, 0x2a, 0x52, 0x12, 0x18, 0x12, 0x0a, 0x1a, 0x02, 0x12, 0x00, 0x22, 0x04, 0x12, 0x00, 0x22,
	0x00, 0x22, 0x04, 0x12, 0x02, 0x10, 0x01, 0x2a, 0x02, 0x12, 0x00, 0x30, 0x01, 0x1a, 0x1a, 0x12,
	0x04, 0x1a, 0x00, 0x22, 0x00, 0x1a, 0x08, 0x12, 0x00, 0x1a, 0x04, 0x10, 0x01, 0x1a, 0x00, 0x22,
	0x00, 0x2a, 0x04, 0x12, 0x02, 0x10, 0x01, 0x3a, 0x00, 0x22, 0x04, 0x12, 0x02, 0x12, 0x00, 0x2a,
	0x14, 0x0a, 0x10, 0x12, 0x04, 0x1a, 0x00, 0x22, 0x00, 0x1a, 0x08, 0x12, 0x00, 0x1a, 0x04, 0x10,
	0x01, 0x1a, 0x00, 0x18, 0x01, 0x42, 0x00, 0x4a, 0x30, 0x12, 0x00, 0x1a, 0x04, 0x0a, 0x00, 0x12,
	0x00, 0x22, 0x26, 0x0a, 0x24, 0x1a, 0x1c, 0x0a, 0x1a, 0x0a, 0x08, 0x2a, 0x02, 0x0a, 0x00, 0x32,
	0x00, 0x3a, 0x00, 0x12, 0x00, 0x1a, 0x0c, 0x0a, 0x08, 0x2a, 0x02, 0x0a, 0x00, 0x32, 0x00, 0x3a,
	0x00, 0x12, 0x00, 0x22, 0x04, 0x0a, 0x02, 0x12, 0x00, 0x5a, 0x0c, 0x12, 0x00, 0x1a, 0x00, 0x22,
	0x06, 0x12, 0x04, 0x08, 0x01, 0x10, 0x02, 0x62, 0x00, 0x72, 0x0c, 0x12, 0x00, 0x1a, 0x00, 0x22,
	0x06, 0x12, 0x04, 0x08, 0x01, 0x10, 0x02, 0x7a, 0x04, 0x0a, 0x00, 0x22, 0x00, 0x8a, 0x01, 0x04,
	0x0a, 0x00, 0x22, 0x00, 0x9a, 0x01, 0x0c, 0x12, 0x00, 0x1a, 0x00, 0x22, 0x06, 0x12, 0x04, 0x08,
	0x01, 0x10, 0x02, 0xaa, 0x01, 0x02, 0x0a, 0x00, 0xb2, 0x01, 0x00, 0xba, 0x01, 0x00, 0xc2, 0x01,
	0x00, 0x12, 0x85, 0x01, 0x0a, 0x2b, 0x12, 0x00, 0x1a, 0x00, 0x22, 0x00, 0x2a, 0x00, 0x32, 0x0c,
	0x0a, 0x00, 0x12, 0x00, 0x1a, 0x00, 0x22, 0x00, 0x2a, 0x00, 0x3a, 0x00, 0x3a, 0x00, 0x42, 0x00,
	0x52, 0x00, 0x62, 0x00, 0x6a, 0x04, 0x12, 0x00, 0x1a, 0x00, 0x7a, 0x02, 0x0a, 0x00, 0x92, 0x01,
	0x00, 0x22, 0x02, 0x0a, 0x00, 0x4a, 0x00, 0x5a, 0x0c, 0x0a, 0x0a, 0x0a, 0x00, 0x22, 0x00, 0x2a,
	0x00, 0x32, 0x00, 0x4a, 0x00, 0x72, 0x24, 0x0a, 0x22, 0x0a, 0x1e, 0x0a, 0x00, 0x12, 0x08, 0x12,
	0x06, 0x0a, 0x02, 0x0a, 0x00, 0x1a, 0x00, 0x1a, 0x10, 0x22, 0x06, 0x0a, 0x02, 0x0a, 0x00, 0x1a,
	0x00, 0x2a, 0x06, 0x0a, 0x02, 0x0a, 0x00, 0x1a, 0x00, 0x12, 0x00, 0x8a, 0x01, 0x00, 0x92, 0x01,
	0x06, 0x0a, 0x00, 0x12, 0x02, 0x0a, 0x00, 0xa2, 0x01, 0x06, 0x12, 0x04, 0x0a, 0x00, 0x12, 0x00,
	0xb2, 0x01, 0x00, 0xba, 0x01, 0x00, 0xc2, 0x01, 0x00, 0x1a, 0xc8, 0x01, 0x12, 0x00, 0x1a, 0x62,
	0x12, 0x00, 0x1a, 0x00, 0x3a, 0x00, 0x42, 0x00, 0x72, 0x02, 0x0a, 0x00, 0x82, 0x01, 0x00, 0x8a,
	0x01, 0x02, 0x12, 0x00, 0x92, 0x01, 0x00, 0x9a, 0x01, 0x00, 0xa2, 0x01, 0x00, 0xaa, 0x01, 0x00,
	0xb2, 0x01, 0x00, 0xba, 0x01, 0x00, 0xda, 0x01, 0x06, 0x0a, 0x00, 0x12, 0x02, 0x0a, 0x00, 0xea,
	0x01, 0x00, 0xf2, 0x01, 0x00, 0xfa, 0x01, 0x00, 0x82, 0x02, 0x00, 0x92, 0x02, 0x00, 0xaa, 0x02,
	0x00, 0xb2, 0x02, 0x00, 0xba, 0x02, 0x00, 0xca, 0x02, 0x00, 0xda, 0x02, 0x02, 0x0a, 0x00, 0xea,
	0x02, 0x04, 0x0a, 0x02, 0x0a, 0x00, 0xf2, 0x02, 0x06, 0x0a, 0x00, 0x12, 0x00, 0x1a, 0x00, 0xfa,
	0x02, 0x00, 0x22, 0x0c, 0x12, 0x00, 0x1a, 0x02, 0x0a, 0x00, 0x22, 0x00, 0x2a, 0x02, 0x0a, 0x00,
	0x3a, 0x00, 0x62, 0x00, 0x6a, 0x00, 0x72, 0x1c, 0x0a, 0x00, 0x12, 0x0c, 0x0a, 0x00, 0x12, 0x02,
	0x0a, 0x00, 0x1a, 0x00, 0x22, 0x02, 0x0a, 0x00, 0x1a, 0x0a, 0x0a, 0x00, 0x12, 0x02, 0x0a, 0x00,
	0x1a, 0x00, 0x22, 0x00, 0x7a, 0x00, 0x82, 0x01, 0x02, 0x0a, 0x00, 0x92, 0x01, 0x00, 0x9a, 0x01,
	0x14, 0x22, 0x02, 0x12, 0x00, 0x32, 0x04, 0x12, 0x00, 0x1a, 0x00, 0x3a, 0x04, 0x12, 0x00, 0x1a,
	0x00, 0x42, 0x00, 0x4a, 0x00, 0xa2, 0x01, 0x00, 0xb2, 0x01, 0x00, 0xc2, 0x01, 0x00, 0xca, 0x01,
	0x00, 0xd2, 0x01, 0x00, 0x32, 0x00, 0x38, 0x02, 0x4a, 0x2a, 0x0a, 0x06, 0x12, 0x04, 0x0a, 0x00,
	0x12, 0x00, 0x12, 0x04, 0x1a, 0x02, 0x10, 0x01, 0x1a, 0x02, 0x12, 0x00, 0x22, 0x00, 0x3a, 0x02,
	0x0a, 0x00, 0x42, 0x0a, 0x08, 0x02, 0x12, 0x06, 0x01, 0x02, 0x03, 0x05, 0x06, 0x07, 0x4a, 0x00,
	0x5a, 0x02, 0x0a, 0x00, 0x58, 0x01, 0x58, 0x02, 0x58, 0x06, 0x62, 0x0c, 0x12, 0x04, 0x0a, 0x00,
	0x12, 0x00, 0x1a, 0x02, 0x0a, 0x00, 0x22, 0x00, 0x6a, 0x00, 0x7a, 0x04, 0x1a, 0x02, 0x08, 0x01,
	0x12, 0x0c, 0x0a, 0x0a, 0x0a, 0x06, 0x0a, 0x02, 0x0a, 0x00, 0x12, 0x00, 0x12, 0x00,
}

// EncodeGetLibraryState builds the request to get the initial library state
// or trigger a delta sync. Uses the full field mask template captured from gpmc.
// Pass empty syncToken for the first full sync.
func EncodeGetLibraryState(syncToken string) []byte {
	if syncToken == "" {
		// Use the template as-is (it already has an empty sync_token at field 1.6)
		return libraryStateTemplate
	}
	// For delta sync, we need to replace the empty sync_token in the template.
	// The template has field 1.6 as an empty string. We rebuild the outer
	// wrapper with the sync_token spliced in by appending it to the field 1 content.
	// Since protobuf merges duplicate fields (last wins for scalars, append for repeated),
	// we can just append the sync_token field after the template's field 1 content.
	var extra pbBuilder
	extra = extra.str(6, syncToken)
	// Wrap: take the template's field 1 content and append our sync_token
	// The template starts with field 1 (tag 0x0a). We need to extract field 1's content,
	// append our extra bytes, and re-wrap.
	root, err := parsePBFields(libraryStateTemplate)
	if err != nil {
		// Fallback: just append to the template bytes (proto3 will merge)
		return append(append([]byte(nil), libraryStateTemplate...), pbBuilder{}.message(1, extra)...)
	}
	f1Content := root.bytesAt(1)
	f2Content := root.bytesAt(2)
	newF1 := append(append([]byte(nil), f1Content...), extra...)
	var b pbBuilder
	b = b.bytes(1, newF1)
	if f2Content != nil {
		b = b.bytes(2, f2Content)
	}
	return b
}

// EncodeGetLibraryPage builds the request to get the next page of library items.
// Uses the same field mask template but with resumeToken at field 1.4 instead of sync_token.
func EncodeGetLibraryPage(resumeToken string) []byte {
	root, err := parsePBFields(libraryStateTemplate)
	if err != nil {
		return nil
	}
	f1Content := root.bytesAt(1)
	f2Content := root.bytesAt(2)

	// Append resume_token (field 4) to the field 1 content
	var extra pbBuilder
	extra = extra.str(4, resumeToken)
	newF1 := append(append([]byte(nil), f1Content...), extra...)

	var b pbBuilder
	b = b.bytes(1, newF1)
	if f2Content != nil {
		b = b.bytes(2, f2Content)
	}
	return b
}

// LibraryItem holds the essential fields from a Google Photos media item.
type LibraryItem struct {
	MediaKey  string
	FileName  string
	SizeBytes int64
	Timestamp int64 // UTC epoch seconds
	IsVideo   bool
}

// msgsAt returns all occurrences of field num as parsed nested messages.
// Use this for repeated message fields.
func (f pbFields) msgsAt(num protowire.Number) []pbFields {
	vs, ok := f[num]
	if !ok {
		return nil
	}
	var result []pbFields
	for _, v := range vs {
		if v.bytes == nil {
			continue
		}
		nested, err := parsePBFields(v.bytes)
		if err != nil {
			continue
		}
		result = append(result, nested)
	}
	return result
}

// DecodeLibraryResponse parses the response from the library listing endpoint.
// Returns the sync/resume tokens for pagination and the list of media items.
func DecodeLibraryResponse(data []byte) (syncToken, resumeToken string, items []LibraryItem, err error) {
	root, err := parsePBFields(data)
	if err != nil {
		return "", "", nil, fmt.Errorf("gotohp: failed to parse library response: %w", err)
	}

	container := root.msg(1)
	if container == nil {
		return "", "", nil, fmt.Errorf("gotohp: library response missing container (field 1)")
	}

	resumeToken = container.strAt(1)
	syncToken = container.strAt(6)

	// Parse media items (field 2, repeated)
	for _, itemFields := range container.msgsAt(2) {
		item := LibraryItem{
			MediaKey: itemFields.strAt(1),
		}

		// Metadata is in field 2
		metadata := itemFields.msg(2)
		if metadata != nil {
			item.FileName = metadata.strAt(4)
			item.SizeBytes = int64(metadata.varintAt(10))
			item.Timestamp = int64(metadata.varintAt(7))
		}

		// Content type is in field 5
		content := itemFields.msg(5)
		if content != nil {
			item.IsVideo = content.varintAt(1) == 1
		}

		if item.MediaKey != "" {
			items = append(items, item)
		}
	}

	return syncToken, resumeToken, items, nil
}
