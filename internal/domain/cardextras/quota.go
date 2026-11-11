package cardextras

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/crueber/weave/internal/ops"
)

// defaultUserStorageMB is the per-user upload quota (WEAVE_USER_STORAGE_MB; 0 = unlimited).
const defaultUserStorageMB = 5 * 1024

func userStorageLimit() int64 {
	mb := int64(defaultUserStorageMB)
	if v := strings.TrimSpace(os.Getenv("WEAVE_USER_STORAGE_MB")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			mb = n
		} else {
			fmt.Println("WEAVE_USER_STORAGE_MB: invalid value", v, "- using", mb)
		}
	}
	return mb << 20
}

// UserStorageUsed is the total size of the files the user has uploaded (links are free).
func UserStorageUsed(e *ops.Env, userID string) int64 {
	var n int64
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COALESCE(SUM(size),0) FROM attachments WHERE creator_user_id=? AND type='file'`, userID).Scan(&n)
	return n
}

// checkUserQuota refuses an upload of add bytes that would push the user past their quota.
func checkUserQuota(e *ops.Env, add int64) error {
	limit := userStorageLimit()
	if limit == 0 {
		return nil
	}
	if used := UserStorageUsed(e, e.User.UserID); used+add > limit {
		return &ops.Error{Status: 413, Code: "storage_quota", Msg: fmt.Sprintf(
			"upload storage quota exceeded: you are using %d of %d MB; delete some attachments or ask an admin to raise WEAVE_USER_STORAGE_MB",
			used>>20, limit>>20)}
	}
	return nil
}
