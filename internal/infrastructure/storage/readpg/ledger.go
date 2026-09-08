package readpg

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/danicotech/hestia/internal/core/platform/adminecon"
	"github.com/danicotech/hestia/internal/core/platform/readmodel"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 帳本分錄的管理端讀取(transport 的 LedgerReader / AdminEconomyService.ListEntries)。
//
// 為什麼這個 port 存在:管理端退款用 entry_ref 定址,但在它之前沒有任何 RPC
// 產得出 entry_ref——管理員只能翻資料庫才叫得動退款。這裡補上缺口。
//
// **token_entries 是 append-only 的事實來源,本檔案只有 SELECT。**

// 游標格式:base64url( "v1|<user_id>|<created_at 的 UnixMicro>|<entry id>" )。
//
// 為什麼是這四段:
//   - 版本前綴讓格式將來能換,舊 token 會被明確拒絕而不是被誤讀成別的意思;
//   - created_at + id 就是 keyset 的排序鍵(見 read.sql),UnixMicro 與
//     Postgres timestamptz 的精度剛好對齊,round-trip 不失真;
//   - **user_id 存進 token 只為了驗證來源**:解碼時比對呼叫端傳進來的 UserID,
//     對不上就拒絕。這不是授權機制——真正的隔離是「查詢的 user_id 永遠取自參數,
//     游標只決定位置」,所以就算有人偽造 token 也不可能讀到別人的分錄;
//     這道比對只是讓「把 A 的 token 貼到 B 的查詢」這種明顯錯誤早點失敗。
//
// 沒有簽章是刻意的:token 不承載任何權限,簽了也擋不住它本來就擋不住的東西,
// 反而多一把要輪替的金鑰。
const (
	cursorVersion    = "v1"
	cursorFieldCount = 4
)

// cursor 是解碼後的游標位置。createdAt 為 nil 表示「從頭開始」(第一頁)。
type cursor struct {
	createdAt *time.Time
	id        int64
}

func encodeCursor(userID int64, createdAt time.Time, id int64) string {
	raw := strings.Join([]string{
		cursorVersion,
		strconv.FormatInt(userID, 10),
		strconv.FormatInt(createdAt.UnixMicro(), 10),
		strconv.FormatInt(id, 10),
	}, "|")
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor 解析 page token。空字串 = 第一頁(非錯誤)。
//
// 任何無法解析的形狀都回 adminecon.ErrInvalidInput(映射成 InvalidArgument),
// 絕不 panic、也絕不「解析失敗就當第一頁」——後者會讓客戶端以為自己在翻頁,
// 實際上一直拿到第一頁,是最難查的那種 bug。
func decodeCursor(token string, userID int64) (cursor, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return cursor{}, nil
	}
	rejected := func(why string) (cursor, error) {
		// 刻意不把 token 原文放進訊息:它是客戶端可控字串,不該進 log。
		return cursor{}, fmt.Errorf("page_token %s: %w", why, adminecon.ErrInvalidInput)
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return rejected("不是合法的 base64url")
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != cursorFieldCount || parts[0] != cursorVersion {
		return rejected("版本或欄位數不符")
	}
	tokenUser, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return rejected("user 欄位不是整數")
	}
	// 來源驗證:游標必須是同一個使用者的查詢發出來的。
	if tokenUser != userID {
		return rejected("與查詢的使用者不符")
	}
	micro, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return rejected("時間欄位不是整數")
	}
	id, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return rejected("分錄 id 不是整數")
	}
	at := time.UnixMicro(micro).UTC()
	return cursor{createdAt: &at, id: id}, nil
}

// ListEntries 依 keyset 游標翻使用者的帳本分錄,新的在前。
//
// 分頁契約:
//   - 排序 (created_at, id) 降冪。**不是**只有 created_at:同一個 transaction
//     寫出的多筆分錄 created_at 完全相同(now() 在 tx 內是常數),
//     沒有 id tie-break 翻頁就會重複或漏抓。
//   - 每次多撈一列來判斷有沒有下一頁:剛好翻完時 NextPageToken 為空,
//     客戶端不會被騙去要一頁空的。
//   - 不用 OFFSET(token_entries 是分區大表,OFFSET 愈翻愈慢);
//     時間範圍條件直接落在分區鍵 created_at 上,planner 會裁掉不相關的分區。
//   - 帳本 append-only,所以翻頁期間已讀過的位置不會變動;新寫入的分錄
//     created_at 更大,只會出現在第一頁之前,不影響游標之後的結果。
func (s *Service) ListEntries(
	ctx context.Context, q readmodel.LedgerEntriesQuery,
) (*readmodel.LedgerEntriesPage, error) {
	if q.UserID <= 0 {
		return nil, fmt.Errorf("user_id 不合法: %w", adminecon.ErrInvalidInput)
	}
	// 入口層已把 PageSize 夾在 1..200;這裡再夾一次是防禦性的
	// (port 的其他呼叫端不見得記得),超界不回錯,直接用上限。
	size := q.PageSize
	if size <= 0 || size > s.limit {
		size = s.limit
	}
	cur, err := decodeCursor(q.PageToken, q.UserID)
	if err != nil {
		return nil, err
	}

	// 多撈一列判斷下一頁;+1 溢位時退回不多撈(等同最後一頁,不會回錯 token)。
	fetch := size + 1
	if fetch <= size {
		fetch = size
	}
	rows, err := s.q.ListLedgerEntries(ctx, db.ListLedgerEntriesParams{
		UserID:          q.UserID,
		After:           q.After,
		Before:          q.Before,
		CursorCreatedAt: cur.createdAt,
		CursorID:        cur.id,
		RowLimit:        fetch,
	})
	if err != nil {
		return nil, fmt.Errorf("列出使用者 %d 的帳本分錄: %w", q.UserID, err)
	}

	hasMore := len(rows) > int(size)
	if hasMore {
		rows = rows[:size]
	}
	page := &readmodel.LedgerEntriesPage{
		Entries: make([]readmodel.LedgerEntryView, 0, len(rows)),
	}
	for _, r := range rows {
		page.Entries = append(page.Entries, readmodel.LedgerEntryView{
			// EntryRef = 內部 id 的十進位字串。token_entries 沒有 public_id,
			// 管理端退款只能用它定址——已裁決的鐵則 5 例外(schemas/05)。
			EntryRef:      strconv.FormatInt(r.ID, 10),
			UserPublicID:  r.UserPublicID,
			Currency:      r.Currency,
			Amount:        r.Amount,
			Reason:        r.Reason,
			CreatedAt:     r.CreatedAt,
			Refunded:      r.Refunded,
			ActorPublicID: r.ActorPublicID,
		})
	}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextPageToken = encodeCursor(q.UserID, last.CreatedAt, last.ID)
	}
	return page, nil
}
