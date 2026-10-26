package terabox

import (
	"encoding/base64"
	"fmt"
	"hash/crc32"
	"io"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"

	"crypto/md5"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/rest"
)

// NewRequest init new request params
func NewRequest(method, path string) *rest.Opts {
	if strings.HasPrefix(path, "https://") {
		return &rest.Opts{Method: method, RootURL: path, Parameters: url.Values{}}
	}
	return &rest.Opts{Method: method, Path: path, Parameters: url.Values{}}
}

// IsInSlice check is slice contain the elem
func IsInSlice[T comparable](v T, list []T) bool {
	if list == nil {
		return false
	}
	for i := 0; i < len(list); i++ {
		if list[i] == v {
			return true
		}
	}
	return false
}

func debug(opt *Options, level uint8, str string, args ...any) {
	if opt.DebugLevel < level {
		return
	}

	fs.Debugf(nil, str, args...)
}

func getStrBetween(raw, start, end string) string {
	regexPattern := fmt.Sprintf(`%s(.*?)%s`, regexp.QuoteMeta(start), regexp.QuoteMeta(end))
	regex := regexp.MustCompile(regexPattern)
	matches := regex.FindStringSubmatch(raw)
	if len(matches) < 2 {
		return ""
	}
	mid := matches[1]
	return mid
}

func sign(s1, s2 string) string {
	var a = make([]int, 256)
	var p = make([]int, 256)
	var o []byte
	var v = len(s1)

	for q := 0; q < 256; q++ {
		a[q] = int(s1[(q % v) : (q%v)+1][0])
		p[q] = q
	}

	for u, q := 0, 0; q < 256; q++ {
		u = (u + p[q] + a[q]) % 256
		p[q], p[u] = p[u], p[q]
	}

	for i, u, q := 0, 0, 0; q < len(s2); q++ {
		i = (i + 1) % 256
		u = (u + p[i]) % 256
		p[i], p[u] = p[u], p[i]
		k := p[((p[i] + p[u]) % 256)]
		o = append(o, byte(int(s2[q])^k))
	}

	return base64.StdEncoding.EncodeToString(o)
}

func valuedCookie(cookie string) string {
	cookie = textproto.TrimString(cookie)
	if len(cookie) > 2 && cookie[0] == '"' && cookie[len(cookie)-1] == '"' {
		cookie = cookie[1 : len(cookie)-1]
	}

	parts := strings.Split(cookie, ";")
	if len(parts) == 1 && !strings.Contains(parts[0], "=") {
		return fmt.Sprintf("ndus=%s; lang=en", parts[0])
	}

	return cookie
}

// decodeMD5 returns the server MD5 in canonical form.
//
// TeraBox's /api/create returns a plain 32 character hex MD5 (verified
// against a real upload: the value matched md5sum of the source file).
// The previous implementation always ran a "decode" transformation on
// 32 character input, which turned that correct value into a different
// hash and made every chunked upload fail its control check.
func decodeMD5(md5 string) string {
	return strings.ToLower(strings.TrimSpace(md5))
}

// isHexMD5 reports whether s is a plain 32 character hexadecimal MD5.
func isHexMD5(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func getChunkSize(fileSize int64, isVIP bool) int64 {
	const MiB = 1024 * 1024
	const GiB = 1024 * MiB

	limitSizes := []int64{4, 8, 16, 32, 64, 128}

	if !isVIP {
		return limitSizes[0] * MiB
	}

	for _, limit := range limitSizes {
		if fileSize <= limit*GiB {
			return limit * MiB
		}
	}

	return limitSizes[len(limitSizes)-1] * MiB
}

// rapidUploadData - hashes of the file needed for the rapid upload
// (server side dedup) attempt and for the precreate/create steps
type rapidUploadData struct {
	sliceMD5     string   // md5 of the first 256KiB of the file
	contentMD5   string   // md5 of the whole file
	contentCrc32 string   // crc32 of the whole file
	blockList    []string // md5 of every chunk
	blockData    [][]byte // raw data of every chunk, kept for the upload step
}

// rapidUploadInfo reads the stream once and computes the hashes needed for
// rapid upload (server-side deduplication) and precreate/create steps, while
// preserving chunk data in memory to avoid re-reading the source stream.
func rapidUploadInfo(in io.Reader, size int64, chunkSize int64) (*rapidUploadData, error) {
	res := &rapidUploadData{}
	contentHash := md5.New()
	crcHash := crc32.NewIEEE()

	chunkNum := 0
	for {
		chunk := make([]byte, chunkSize)
		n, err := io.ReadFull(in, chunk)
		data := chunk[:n]
		if n > 0 {
			res.blockData = append(res.blockData, data)
			res.blockList = append(res.blockList, fmt.Sprintf("%x", md5.Sum(data)))
			contentHash.Write(data)
			_, _ = crcHash.Write(data)
			if chunkNum == 0 {
				sliceLen := int64(256 * 1024)
				if int64(n) < sliceLen {
					sliceLen = int64(n)
				}
				res.sliceMD5 = fmt.Sprintf("%x", md5.Sum(data[:sliceLen]))
			}
			chunkNum++
		}
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}
	}

	if chunkNum == 0 {
		emptyMD5 := fmt.Sprintf("%x", md5.Sum(nil))
		res.blockList = []string{emptyMD5}
		res.blockData = [][]byte{{}}
		res.sliceMD5 = emptyMD5
	}
	res.contentMD5 = fmt.Sprintf("%x", contentHash.Sum(nil))
	res.contentCrc32 = fmt.Sprintf("%d", crcHash.Sum32())

	return res, nil
}
