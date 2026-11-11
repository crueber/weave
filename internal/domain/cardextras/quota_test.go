package cardextras_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/cardextras"
	"github.com/crueber/weave/internal/ops"
)

func TestUserStorageQuota(t *testing.T) {
	t.Setenv("WEAVE_USER_STORAGE_MB", "1")
	e := fixture(t)
	chunk := func() *ops.File { return &ops.File{Name: "a.bin", Data: bytes.Repeat([]byte{1, 2, 3, 4}, 150<<10)} } // 600 KB
	first := call[cardextras.AttachmentItem](t, e, "attachment.create", cardextras.AttachmentCreateIn{CardID: "c1", File: chunk()})
	if got := cardextras.UserStorageUsed(e, e.User.UserID); got != 600<<10 {
		t.Fatalf("usage = %d", got)
	}
	_, err := ops.Typed[any](e, "attachment.create", cardextras.AttachmentCreateIn{CardID: "c1", File: chunk()})
	var oe *ops.Error
	if err == nil || !errors.As(err, &oe) || oe.Code != "storage_quota" || oe.Status != 413 || !strings.Contains(oe.Msg, "WEAVE_USER_STORAGE_MB") {
		t.Fatalf("second upload over quota: %v", err)
	}
	// links never count
	call[cardextras.AttachmentItem](t, e, "attachment.create", cardextras.AttachmentCreateIn{CardID: "c1", URL: "https://example.com/x"})
	// deleting frees the quota again
	call[any](t, e, "attachment.delete", cardextras.IDIn{ID: first.ID})
	call[cardextras.AttachmentItem](t, e, "attachment.create", cardextras.AttachmentCreateIn{CardID: "c1", File: chunk()})

	t.Setenv("WEAVE_USER_STORAGE_MB", "0") // unlimited
	for i := 0; i < 3; i++ {
		call[cardextras.AttachmentItem](t, e, "attachment.create", cardextras.AttachmentCreateIn{CardID: "c1", File: chunk()})
	}
}

func TestUserStorageQuotaDefaultIsGenerous(t *testing.T) {
	e := fixture(t) // no env: 5 GiB default; a 10 MB upload is fine
	call[cardextras.AttachmentItem](t, e, "attachment.create", cardextras.AttachmentCreateIn{CardID: "c1", File: &ops.File{Name: "big.bin", Data: make([]byte, 10<<20)}})
}
