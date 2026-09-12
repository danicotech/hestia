package tournament

import (
	"context"
	"fmt"
	"sort"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/bracket"
)

// Service 是賽事階段與抽籤的唯一入口。
//
// 它只依賴 Repo(port),不認識 SQL 也不認識 transport ——
// 於是「階段機的規則」可以在不啟動資料庫的情況下被窮舉測試,
// 而那正是這套規則唯一值得信任的理由。
type Service struct {
	repo Repo
}

// NewService 建立賽事服務。
func NewService(repo Repo) *Service {
	return &Service{repo: repo}
}

// View 是一屆賽事加上解析好的設定。
//
// 綁在一起回傳是因為呼叫端幾乎沒有「只要 Tournament 不要 Config」的場合:
// 規則頁要段位措辭、抽籤要 bp_per_rank_gap、下注要 max_stake。
// 分開兩個方法的結果是每個呼叫端都要記得呼叫第二次,而忘記的那次會拿到零值。
type View struct {
	Tournament Tournament
	Config     Config
	// ConfigErr 非 nil 表示 config 有壞欄位,已逐項退回預設值(見 ParseConfig)。
	// **Config 仍然可用**;呼叫端該做的是記 log 或在後台標紅,不是中止。
	ConfigErr error
}

// Get 讀一屆賽事並解析設定。
func (s *Service) Get(ctx context.Context, slug string) (*View, error) {
	t, err := s.repo.TournamentBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	cfg, cfgErr := ParseConfig(t.ConfigRaw)
	return &View{Tournament: t, Config: cfg, ConfigErr: cfgErr}, nil
}

// AdvanceParams 是一次階段推進。
type AdvanceParams struct {
	Slug string
	// To 是目標階段。刻意要求呼叫端指名,而不是提供一個「下一步」按鈕:
	// 前端顯示的階段可能是幾分鐘前讀到的,指名才能在後端比對出不一致。
	To Phase
	// ActorUserID 是執行的裁判。必填 —— 這個動作要進 admin_audit_logs。
	ActorUserID int64
	Reason      string
}

// AdvancePhase 推進(或退回)賽事階段。
//
// 合法的轉換只有兩種:往前一步,以及 drawing → ranked 這條唯一的回頭路。
// 規則本身在 CanAdvance,這裡負責的是三件 CanAdvance 不知道的事:
//
//  1. 進入 drawing 前必須所有 active 選手都評過段 —— 未評段者算不出 BP,
//     硬抽的結果是那幾場悄悄變成沒有讓武,而對戰表上看不出異常。
//  2. 退回 ranked 要順手作廢對戰表與籤位(由 RollbackToRanked 在同一個 tx 做)。
//  3. 寫入以「讀到的階段」為條件,兩個裁判同搶時只有一個會成功。
func (s *Service) AdvancePhase(ctx context.Context, p AdvanceParams) (*View, error) {
	if p.ActorUserID == 0 {
		return nil, ErrActorRequired
	}
	v, err := s.Get(ctx, p.Slug)
	if err != nil {
		return nil, err
	}
	from := v.Tournament.Phase
	if err := CanAdvance(from, p.To); err != nil {
		return nil, err
	}

	if p.To == PhaseDrawing {
		if err := s.requireAllRanked(ctx, v.Tournament.ID); err != nil {
			return nil, err
		}
	}

	if IsRollback(from, p.To) {
		err = s.repo.RollbackToRanked(ctx, RollbackParams{
			TournamentID: v.Tournament.ID,
			ActorUserID:  p.ActorUserID,
			Reason:       p.Reason,
		})
	} else {
		err = s.repo.UpdatePhase(ctx, UpdatePhaseParams{
			TournamentID: v.Tournament.ID,
			From:         from,
			To:           p.To,
			ActorUserID:  p.ActorUserID,
			Reason:       p.Reason,
		})
	}
	if err != nil {
		return nil, err
	}

	v.Tournament.Phase = p.To
	return v, nil
}

// SetRankParams 是裁判評定或修改一位選手的段位。
type SetRankParams struct {
	Slug           string
	PlayerPublicID string
	Rank           bp.Rank
	ActorUserID    int64
	Reason         string
}

// SetRank 寫入一位選手的本屆段位。
//
// 只在 ranking(評段中)與 ranked(公布後的異議期)開放。
// 進入 drawing 之後回 ErrRanksLocked 而不是一般的 ErrWrongPhase ——
// 兩者要分得出來,因為前者有明確的補救動作(退回 ranked 重抽),
// 而那個動作必須在錯誤訊息裡講清楚,否則裁判只會看到「現在不能改」然後卡住。
func (s *Service) SetRank(ctx context.Context, p SetRankParams) (*Player, error) {
	if p.ActorUserID == 0 {
		return nil, ErrActorRequired
	}
	if !p.Rank.Valid() {
		return nil, fmt.Errorf("%w: %d", ErrInvalidRank, p.Rank)
	}
	t, err := s.repo.TournamentBySlug(ctx, p.Slug)
	if err != nil {
		return nil, err
	}
	if !RanksEditable(t.Phase) {
		// 抽籤(含)之後是「鎖定」,報名期則只是「還沒到時候」。
		if t.Phase == PhaseDrawing || t.Phase == PhaseInProgress || t.Phase == PhaseFinished {
			return nil, fmt.Errorf("%w(目前為 %s)", ErrRanksLocked, t.Phase.Label())
		}
		return nil, RequirePhase(t.Phase, PhaseRanking, PhaseRanked)
	}

	player, err := s.repo.PlayerByPublicID(ctx, t.ID, p.PlayerPublicID)
	if err != nil {
		return nil, err
	}
	updated, err := s.repo.SetPlayerRank(ctx, SetPlayerRankParams{
		TournamentID: t.ID,
		PlayerID:     player.ID,
		Rank:         p.Rank,
		ActorUserID:  p.ActorUserID,
		Reason:       p.Reason,
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// DrawParams 是一次抽籤(重抽也走這裡)。
type DrawParams struct {
	Slug string
	// Seed 留空 = 產生一個新的隨機種子。
	// 填值則是**重現**一次既有的抽籤(16 位 hex,見 DrawSeed)——
	// 裁判被質疑籤表時,拿舊種子重跑應得到位元相同的結果。
	Seed        string
	ActorUserID int64
	Reason      string
}

// DrawResult 是一次抽籤的結果。
type DrawResult struct {
	// Seed 是這次使用的種子,已寫進稽核紀錄。前端要顯示給裁判看。
	Seed DrawSeed
	// Seats 依籤位遞增排序。
	Seats []SeatAssignment
	// Matches 依 (Round, Slot) 排序。
	Matches []MatchSeat
	// ByePlayerIDs 是首輪輪空者(隨機落的)。
	ByePlayerIDs []int64
	// TotalRounds 是總輪數 = log2(補齊後的樹寬)。
	TotalRounds int
	// Players 是實際進入這張表的選手,依籤位排序。
	// 一併回傳讓呼叫端不必為了顯示名字再查一次。
	Players []Player
}

// Draw 抽籤:把 active 且已評段的選手排進一張單淘汰對戰表,整批寫回。
//
// 重抽就是再呼叫一次 —— ReplaceDraw 會先清掉舊的籤位與場次。
// 「確認」不是這裡的動作,而是 AdvancePhase(drawing → in_progress):
// 離開 drawing 階段就等於鎖定,不需要另一個 confirmed 旗標。
// 少一個旗標就少一個「旗標說確認了但階段還在 drawing」的矛盾狀態(鐵則 9)。
func (s *Service) Draw(ctx context.Context, p DrawParams) (*DrawResult, error) {
	if p.ActorUserID == 0 {
		return nil, ErrActorRequired
	}
	t, err := s.repo.TournamentBySlug(ctx, p.Slug)
	if err != nil {
		return nil, err
	}
	if err := RequirePhase(t.Phase, PhaseDrawing); err != nil {
		return nil, err
	}
	if err := s.requireAllRanked(ctx, t.ID); err != nil {
		return nil, err
	}

	players, err := s.repo.ListDrawablePlayers(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	if len(players) == 0 {
		return nil, ErrNoDrawablePlayers
	}

	seed, err := resolveSeed(p.Seed)
	if err != nil {
		return nil, err
	}

	ids := make([]int64, len(players))
	byID := make(map[int64]Player, len(players))
	for i, pl := range players {
		ids[i] = pl.ID
		byID[pl.ID] = pl
	}

	b, err := bracket.Build(ids, seed.Rand())
	if err != nil {
		return nil, fmt.Errorf("建立對戰表: %w", err)
	}

	seats := make([]SeatAssignment, 0, len(b.Seeds))
	for id, no := range b.Seeds {
		seats = append(seats, SeatAssignment{PlayerID: id, SeedNo: int32(no)})
	}
	// 依籤位排序:map 的走訪順序每次都不同,不排的話同一個種子會寫出
	// 順序不同的 Seats,稽核比對與測試都會變得沒有意義。
	sort.Slice(seats, func(i, j int) bool { return seats[i].SeedNo < seats[j].SeedNo })

	matches := make([]MatchSeat, len(b.Matches))
	for i, m := range b.Matches {
		matches[i] = MatchSeat{
			Round:      int32(m.Round),
			Slot:       int32(m.Slot),
			P1PlayerID: m.P1,
			P2PlayerID: m.P2,
		}
	}

	if err := s.repo.ReplaceDraw(ctx, ReplaceDrawParams{
		TournamentID: t.ID,
		Seed:         seed.String(),
		Seats:        seats,
		Matches:      matches,
		ByePlayerIDs: b.Byes,
		ActorUserID:  p.ActorUserID,
		Reason:       p.Reason,
	}); err != nil {
		return nil, err
	}

	ordered := make([]Player, 0, len(seats))
	for _, st := range seats {
		pl := byID[st.PlayerID]
		pl.SeedNo = st.SeedNo
		ordered = append(ordered, pl)
	}

	return &DrawResult{
		Seed:         seed,
		Seats:        seats,
		Matches:      matches,
		ByePlayerIDs: b.Byes,
		TotalRounds:  b.TotalRounds,
		Players:      ordered,
	}, nil
}

// SwapParams 是交換兩位選手的籤位。
type SwapParams struct {
	Slug            string
	PlayerAPublicID string
	PlayerBPublicID string
	ActorUserID     int64
	Reason          string
}

// SwapSeeds 交換兩位選手的籤位。
//
// 存在的理由是重抽太鈍:裁判通常只是想把「同公會的兩個人首輪就碰頭」拆開,
// 而重抽會把整張表打散,連已經公告出去的其他場次一起變動。
//
// 只在 drawing 階段開放,理由與改段位相同 —— 離開 drawing 就代表籤表已確認,
// 而 match_budgets 是依確認後的配對發的。
func (s *Service) SwapSeeds(ctx context.Context, p SwapParams) (*Player, *Player, error) {
	if p.ActorUserID == 0 {
		return nil, nil, ErrActorRequired
	}
	t, err := s.repo.TournamentBySlug(ctx, p.Slug)
	if err != nil {
		return nil, nil, err
	}
	if err := RequirePhase(t.Phase, PhaseDrawing); err != nil {
		return nil, nil, err
	}

	a, err := s.repo.PlayerByPublicID(ctx, t.ID, p.PlayerAPublicID)
	if err != nil {
		return nil, nil, err
	}
	b, err := s.repo.PlayerByPublicID(ctx, t.ID, p.PlayerBPublicID)
	if err != nil {
		return nil, nil, err
	}
	if a.ID == b.ID {
		return nil, nil, ErrSamePlayer
	}
	// 沒有籤位的人不能「交換」—— 那會變成把對方的籤位讓出去,
	// 留下一張少一個人的對戰表,而且沒有任何約束會擋。
	if a.SeedNo == 0 {
		return nil, nil, fmt.Errorf("%w: %s", ErrPlayerNotSeeded, a.PublicID)
	}
	if b.SeedNo == 0 {
		return nil, nil, fmt.Errorf("%w: %s", ErrPlayerNotSeeded, b.PublicID)
	}

	if err := s.repo.SwapSeeds(ctx, SwapSeedsParams{
		TournamentID: t.ID,
		PlayerAID:    a.ID,
		PlayerBID:    b.ID,
		ActorUserID:  p.ActorUserID,
		Reason:       p.Reason,
	}); err != nil {
		return nil, nil, err
	}

	a.SeedNo, b.SeedNo = b.SeedNo, a.SeedNo
	return &a, &b, nil
}

// requireAllRanked 是「未評段者不得進入抽籤」的唯一檢查點。
func (s *Service) requireAllRanked(ctx context.Context, tournamentID int64) error {
	n, err := s.repo.CountUnrankedActivePlayers(ctx, tournamentID)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: 還有 %d 位", ErrPlayersUnranked, n)
	}
	return nil
}

// resolveSeed 把請求裡的種子字串變成 DrawSeed;空字串代表要一個新的。
func resolveSeed(s string) (DrawSeed, error) {
	if s == "" {
		return NewDrawSeed()
	}
	return ParseDrawSeed(s)
}
