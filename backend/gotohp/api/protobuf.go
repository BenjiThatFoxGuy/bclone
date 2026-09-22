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

// emptyMsg is a convenience for building empty nested messages (field mask entries).
func emptyMsg() pbBuilder { return pbBuilder{} }

// buildFieldMask constructs the media item field mask (field 1.1 of the request).
// This tells the server which media item fields to include in the response.
func buildFieldMask() pbBuilder {
	// Field 1.1.1 = basic item fields mask
	var itemFields pbBuilder
	itemFields = itemFields.message(1, emptyMsg())  // media_key
	itemFields = itemFields.message(3, emptyMsg())  // caption
	itemFields = itemFields.message(4, emptyMsg())  // file_name
	itemFields = itemFields.message(5, emptyMsg().  // properties
					message(1, emptyMsg()).
					message(2, emptyMsg()).
					message(3, emptyMsg()).
					message(4, emptyMsg()).
					message(5, emptyMsg()).
					message(7, emptyMsg()))
	itemFields = itemFields.message(6, emptyMsg())  // unknown
	itemFields = itemFields.message(7, emptyMsg().  // dimensions/EXIF
					message(1, emptyMsg()).
					message(2, emptyMsg()))
	itemFields = itemFields.message(8, emptyMsg())  // unknown
	itemFields = itemFields.message(9, emptyMsg())  // server_creation_timestamp
	itemFields = itemFields.message(10, emptyMsg(). // size info
					message(1, emptyMsg()))
	itemFields = itemFields.message(11, emptyMsg()) // upload_status

	// Field 1.1 = content type mask
	var contentMask pbBuilder
	contentMask = contentMask.message(1, itemFields) // item fields
	contentMask = contentMask.message(2, emptyMsg()) // photo data
	contentMask = contentMask.message(3, emptyMsg()) // video data
	contentMask = contentMask.message(4, emptyMsg()) // unknown

	// Outer field 1
	var f1 pbBuilder
	f1 = f1.message(1, contentMask)               // media item mask
	f1 = f1.message(2, emptyMsg().                 // additional fields
			message(1, emptyMsg()).
			message(2, emptyMsg()).
			message(3, emptyMsg()))
	f1 = f1.message(3, emptyMsg().                 // more fields
			message(1, emptyMsg()).
			message(2, emptyMsg()))
	f1 = f1.message(5, emptyMsg().                 // unknown
			message(1, emptyMsg()).
			message(5, emptyMsg().
				message(1, emptyMsg()).
				message(2, emptyMsg())))
	f1 = f1.message(9, emptyMsg().                 // unknown
			message(1, emptyMsg()).
			message(2, emptyMsg()))
	f1 = f1.message(11, emptyMsg().message(1, emptyMsg()))
	f1 = f1.message(12, emptyMsg().message(1, emptyMsg()))
	f1 = f1.message(13, emptyMsg().message(1, emptyMsg().message(1, emptyMsg())))
	f1 = f1.message(15, emptyMsg().message(1, emptyMsg()).message(2, emptyMsg()))
	f1 = f1.message(18, emptyMsg().message(1, emptyMsg()))
	f1 = f1.message(19, emptyMsg().message(1, emptyMsg()))
	f1 = f1.message(20, emptyMsg().message(1, emptyMsg()).message(2, emptyMsg()).message(3, emptyMsg()))
	f1 = f1.message(21, emptyMsg().message(1, emptyMsg()).message(2, emptyMsg()))
	f1 = f1.message(22, emptyMsg().message(1, emptyMsg()))
	f1 = f1.message(25, emptyMsg().message(1, emptyMsg()))

	return f1
}

// EncodeGetLibraryState builds the request to get the initial library state
// or trigger a delta sync. Pass empty syncToken for the first full sync.
func EncodeGetLibraryState(syncToken string) []byte {
	f1 := buildFieldMask()
	if syncToken != "" {
		f1 = f1.str(6, syncToken)
	}
	f1 = f1.varint(7, 2)

	var b pbBuilder
	b = b.message(1, f1)
	return b
}

// EncodeGetLibraryPage builds the request to get the next page of library items.
// Pass the resumeToken from the previous response.
func EncodeGetLibraryPage(resumeToken string) []byte {
	f1 := buildFieldMask()
	f1 = f1.str(4, resumeToken)
	f1 = f1.varint(7, 2)

	var b pbBuilder
	b = b.message(1, f1)
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
