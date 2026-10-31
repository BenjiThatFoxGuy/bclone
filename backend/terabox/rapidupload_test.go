package terabox

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"hash/crc32"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRapidUploadInfo(t *testing.T) {
	// empty file
	data, err := rapidUploadInfo(bytes.NewReader(nil), 0, 4*1024*1024)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%x", md5.Sum(nil)), data.sliceMD5)
	assert.Equal(t, fmt.Sprintf("%x", md5.Sum(nil)), data.contentMD5)
	assert.Equal(t, "0", data.contentCrc32)
	assert.Equal(t, []string{fmt.Sprintf("%x", md5.Sum(nil))}, data.blockList)
	assert.Len(t, data.blockData, 1)

	// small single-chunk file
	content := bytes.Repeat([]byte("terabox"), 1000)
	data, err = rapidUploadInfo(bytes.NewReader(content), int64(len(content)), 4*1024*1024)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%x", md5.Sum(content)), data.contentMD5)
	assert.Equal(t, fmt.Sprintf("%d", crc32.ChecksumIEEE(content)), data.contentCrc32)

	assert.Equal(t, fmt.Sprintf("%x", md5.Sum(content)), data.sliceMD5)
	assert.Equal(t, []string{fmt.Sprintf("%x", md5.Sum(content))}, data.blockList)
	require.Len(t, data.blockData, 1)
	assert.Equal(t, content, data.blockData[0])

	// multi-chunk file
	chunkSize := int64(4 * 1024 * 1024)
	big := make([]byte, 0, 2*chunkSize+12345)
	for i := int64(0); i < 2*chunkSize+12345; i++ {
		big = append(big, byte(i%251))
	}
	data, err = rapidUploadInfo(bytes.NewReader(big), int64(len(big)), chunkSize)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%x", md5.Sum(big)), data.contentMD5)
	assert.Equal(t, fmt.Sprintf("%d", crc32.ChecksumIEEE(big)), data.contentCrc32)
	assert.Equal(t, fmt.Sprintf("%x", md5.Sum(big[:256*1024])), data.sliceMD5)
	require.Len(t, data.blockList, 3)
	assert.Equal(t, fmt.Sprintf("%x", md5.Sum(big[:chunkSize])), data.blockList[0])
	assert.Equal(t, fmt.Sprintf("%x", md5.Sum(big[chunkSize:2*chunkSize])), data.blockList[1])
	assert.Equal(t, fmt.Sprintf("%x", md5.Sum(big[2*chunkSize:])), data.blockList[2])
	require.Len(t, data.blockData, 3)
	assert.Equal(t, big[:chunkSize], data.blockData[0])
	assert.Equal(t, big[chunkSize:2*chunkSize], data.blockData[1])
	assert.Equal(t, big[2*chunkSize:], data.blockData[2])
}
