package terabox

import (
	"errors"
	"testing"
)

func TestCheckRemoteName(t *testing.T) {
	for _, name := range []string{
		"조직도 인쇄·엑셀·PPT 저장 기능 구현.md",
		"plain.txt",
		"✅ ⭐ allowed-bmp.md",
		"dir/파일 이름.md",
	} {
		if err := checkRemoteName(name); err != nil {
			t.Errorf("checkRemoteName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{
		"📋 조직도.md",
		"dir/😀 file.md",
		"𝄞 music.md",
		"𠀀 han.md",
	} {
		err := checkRemoteName(name)
		if err == nil {
			t.Errorf("checkRemoteName(%q) = nil, want error", name)
			continue
		}
		if !errors.Is(err, errUnsupportedName) {
			t.Errorf("checkRemoteName(%q) error = %v, want errUnsupportedName", name, err)
		}
	}
}
