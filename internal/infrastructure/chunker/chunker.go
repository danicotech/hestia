// Package chunker 把 message_logs 聚合成 conversation_chunks(schemas/12-ai-corpus.md)。
//
// 為什麼要聚合:單則訊息做 embedding 太碎、沒有檢索價值(grill Q16)。同一段對話
// 合併起來才是一個語意單位。合併後的 chunk 是去識別化的長期資料——原文與「誰說的」
// 隨 message_logs 的 365 天保留期消失,語意內容以只有內部 id 的形式長存。
//
// 這個套件只有一個 job:BuildChunks,由 maintenance runner 每 10 分鐘叫一次。
// 全部工作在一句 SQL 內完成(queries/chunker.sql),Go 這層只負責讀設定、
// 開交易、拿 advisory lock。
package chunker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
)

// 門檻的預設值。schemas/12 只寫了「極短對話」與「純表情」要排除,沒有給數字;
// 這裡定死並說明理由,不新增 economy_configs key(未經定案的設定就是新概念,專案規則 9)。
const (
	// DefaultMinMessageChars:一則訊息剝掉自訂表情與 mention 後,至少要有這麼多個
	// 有意義字元(alnum,CJK 算數)才算一次發言。取 2 的理由:1 個字的回應
	// (「w」「?」「笑」)是反應而不是內容,而反應的權威位置是 reaction_events;
	// 2 個字已經能承載完整的中文回應(「同意」「不會」),砍掉它們會傷到對話的語意連貫。
	DefaultMinMessageChars = 2
	// DefaultMinMessages:少於 3 則合格訊息的不算「對話」。1 則是獨白、2 則多半是
	// 呼叫與回應,兩者嵌入後只會變成檢索雜訊。
	DefaultMinMessages = 3
	// DefaultMinChunkChars:合併後的文字太短一樣沒有檢索價值。40 字元約等於
	// 三句短中文,是「這段對話有講到東西」的下限。
	DefaultMinChunkChars = 40
)

// gapConfigKey 是斷句時間窗的權威位置(economy_configs,seed 值 20 分鐘)。
const gapConfigKey = "chunk_gap_minutes"

// Service 聚合對話 chunk。門檻是欄位而不是常數,測試可以調;正式部署用 New 的預設值。
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries

	MinMessageChars int32
	MinMessages     int32
	MinChunkChars   int32
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{
		pool:            pool,
		q:               db.New(pool),
		MinMessageChars: DefaultMinMessageChars,
		MinMessages:     DefaultMinMessages,
		MinChunkChars:   DefaultMinChunkChars,
	}
}

// BuildChunks 產生所有「已封閉」的對話 chunk,回傳新增的 chunk 數。
// 簽名與其他 maintenance job 一致,可直接註冊。
//
// 交易邊界:advisory lock 與 INSERT 必須在同一個交易裡。鎖是交易級的,
// commit(或 rollback)時自動釋放——不會像 session 級的鎖那樣在連線被歸還後還掛著。
func (s *Service) BuildChunks(ctx context.Context) (int, error) {
	gap, err := s.gapMinutes(ctx)
	if err != nil {
		return 0, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("開 transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)
	if err := q.LockChunkBuild(ctx); err != nil {
		return 0, fmt.Errorf("取 chunker advisory lock: %w", err)
	}
	n, err := q.InsertConversationChunks(ctx, db.InsertConversationChunksParams{
		GapMinutes:      gap,
		MinMessages:     s.MinMessages,
		MinChunkChars:   s.MinChunkChars,
		MinMessageChars: s.MinMessageChars,
	})
	if err != nil {
		return 0, fmt.Errorf("聚合 conversation_chunks: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return int(n), nil
}

// gapMinutes 讀 economy_configs 目前生效的斷句窗。
// 缺值或不合理直接報錯,不用寫死的預設頂上:gap 決定對話怎麼切,拿一個沒人確認過的
// 數字默默切一年份語料,比停下來要糟(同 maintenance.configDays 的取捨)。
func (s *Service) gapMinutes(ctx context.Context) (int32, error) {
	raw, err := s.q.GetCurrentConfig(ctx, gapConfigKey)
	if err != nil {
		return 0, fmt.Errorf("讀設定 %s: %w", gapConfigKey, err)
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("設定 %s 不是整數 (%s): %w", gapConfigKey, raw, err)
	}
	if v < 1 || v > 24*60 {
		return 0, fmt.Errorf("設定 %s 超出合理範圍(1..1440 分鐘): %d", gapConfigKey, v)
	}
	return int32(v), nil
}
