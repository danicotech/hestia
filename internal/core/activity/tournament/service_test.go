package tournament

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/bp"
)

// fakeRepo 是 Repo 的記憶體實作。
//
// 用 fake 而不是 mock 框架:這些測試要驗的是「service 對 repo 下了什麼指令」
// 與「service 怎麼組裝結果」,而那兩件事看參數記錄就夠了。
// 真正需要資料庫才測得出來的事(UNIQUE 撞鍵、同一個 tx 裡的原子性)
// 屬於 adapter 的測試,不在這一層假裝測過。
type fakeRepo struct {
	tournament Tournament
	players    []Player
	unranked   int64

	// 記錄下來的呼叫,讓測試驗證 service 真的下了對的指令。
	updatePhaseCalls []UpdatePhaseParams
	rollbackCalls    []RollbackParams
	drawCalls        []ReplaceDrawParams
	swapCalls        []SwapSeedsParams
	setRankCalls     []SetPlayerRankParams

	createCalls []CreateTournamentParams

	// 注入的錯誤。
	tournamentErr  error
	updatePhaseErr error
	drawErr        error
	createErr      error
}

func (f *fakeRepo) Create(_ context.Context, p CreateTournamentParams) (Tournament, error) {
	f.createCalls = append(f.createCalls, p)
	if f.createErr != nil {
		return Tournament{}, f.createErr
	}
	return Tournament{
		ID: 900, PublicID: "01J0TOURNAMENT", Slug: p.Slug, Name: p.Name,
		CommunityID: 1, Phase: PhaseSignup, ConfigRaw: p.ConfigRaw,
		SignupBonus: p.SignupBonus,
	}, nil
}

func (f *fakeRepo) TournamentBySlug(_ context.Context, slug string) (Tournament, error) {
	if f.tournamentErr != nil {
		return Tournament{}, f.tournamentErr
	}
	if slug != f.tournament.Slug {
		return Tournament{}, ErrTournamentNotFound
	}
	return f.tournament, nil
}

func (f *fakeRepo) CountUnrankedActivePlayers(context.Context, int64) (int64, error) {
	return f.unranked, nil
}

func (f *fakeRepo) ListDrawablePlayers(context.Context, int64) ([]Player, error) {
	out := make([]Player, len(f.players))
	copy(out, f.players)
	return out, nil
}

func (f *fakeRepo) PlayerByPublicID(_ context.Context, _ int64, publicID string) (Player, error) {
	for _, p := range f.players {
		if p.PublicID == publicID {
			return p, nil
		}
	}
	return Player{}, ErrPlayerNotFound
}

func (f *fakeRepo) UpdatePhase(_ context.Context, p UpdatePhaseParams) error {
	f.updatePhaseCalls = append(f.updatePhaseCalls, p)
	if f.updatePhaseErr != nil {
		return f.updatePhaseErr
	}
	f.tournament.Phase = p.To
	return nil
}

func (f *fakeRepo) RollbackToRanked(_ context.Context, p RollbackParams) error {
	f.rollbackCalls = append(f.rollbackCalls, p)
	f.tournament.Phase = PhaseRanked
	for i := range f.players {
		f.players[i].SeedNo = 0
	}
	return nil
}

func (f *fakeRepo) ReplaceDraw(_ context.Context, p ReplaceDrawParams) error {
	f.drawCalls = append(f.drawCalls, p)
	if f.drawErr != nil {
		return f.drawErr
	}
	byID := make(map[int64]int32, len(p.Seats))
	for _, s := range p.Seats {
		byID[s.PlayerID] = s.SeedNo
	}
	for i := range f.players {
		f.players[i].SeedNo = byID[f.players[i].ID]
	}
	return nil
}

func (f *fakeRepo) SwapSeeds(_ context.Context, p SwapSeedsParams) error {
	f.swapCalls = append(f.swapCalls, p)
	var a, b *Player
	for i := range f.players {
		switch f.players[i].ID {
		case p.PlayerAID:
			a = &f.players[i]
		case p.PlayerBID:
			b = &f.players[i]
		}
	}
	if a == nil || b == nil {
		return ErrPlayerNotFound
	}
	a.SeedNo, b.SeedNo = b.SeedNo, a.SeedNo
	return nil
}

func (f *fakeRepo) SetPlayerRank(_ context.Context, p SetPlayerRankParams) (Player, error) {
	f.setRankCalls = append(f.setRankCalls, p)
	for i := range f.players {
		if f.players[i].ID == p.PlayerID {
			f.players[i].Rank = p.Rank
			return f.players[i], nil
		}
	}
	return Player{}, ErrPlayerNotFound
}

const (
	testSlug  = "2026-baiye-shifeng"
	testActor = int64(77)
)

// newFake 建一屆有 n 位已評段選手的賽事。
func newFake(phase Phase, n int) *fakeRepo {
	f := &fakeRepo{
		tournament: Tournament{
			ID: 1, PublicID: "T1", Slug: testSlug, Name: "百業試鋒",
			CommunityID: 1, Phase: phase, ConfigRaw: []byte(`{"bp_per_rank_gap":8}`),
		},
	}
	for i := range n {
		f.players = append(f.players, Player{
			ID:           int64(i + 1),
			PublicID:     fmt.Sprintf("P%02d", i+1),
			TournamentID: 1,
			FencerID:     int64(i + 1),
			GameID:       fmt.Sprintf("fencer-%02d", i+1),
			DisplayName:  fmt.Sprintf("選手%02d", i+1),
			// 四段輪流分配,讓抽出來的表一定有段位差。
			Rank:   bp.Rank(i%4 + 1),
			Status: PlayerActive,
		})
	}
	return f
}

func TestGetParsesConfig(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseSignup, 0)
	f.tournament.ConfigRaw = []byte(`{"bp_per_rank_gap":12}`)
	v, err := NewService(f).Get(context.Background(), testSlug)
	if err != nil {
		t.Fatal(err)
	}
	if v.Config.BPPerRankGap != 12 {
		t.Errorf("設定沒解析進來:%+v", v.Config)
	}
	if v.ConfigErr != nil {
		t.Errorf("合法設定不該有 ConfigErr:%v", v.ConfigErr)
	}
}

// TestGetSurvivesBrokenConfig 壞設定不該讓整屆賽事打不開。
func TestGetSurvivesBrokenConfig(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseSignup, 0)
	f.tournament.ConfigRaw = []byte(`{"bp_per_rank_gap":0}`)
	v, err := NewService(f).Get(context.Background(), testSlug)
	if err != nil {
		t.Fatalf("壞設定不該讓 Get 失敗:%v", err)
	}
	if !errors.Is(v.ConfigErr, ErrConfigMalformed) {
		t.Errorf("ConfigErr 要是 ErrConfigMalformed,得到 %v", v.ConfigErr)
	}
	if v.Config.BPPerRankGap != DefaultBPPerRankGap {
		t.Errorf("壞設定要退回預設 %d,得到 %d", DefaultBPPerRankGap, v.Config.BPPerRankGap)
	}
}

func TestGetNotFound(t *testing.T) {
	t.Parallel()

	_, err := NewService(newFake(PhaseSignup, 0)).Get(context.Background(), "不存在的賽事")
	if !errors.Is(err, ErrTournamentNotFound) {
		t.Errorf("要回 ErrTournamentNotFound,得到 %v", err)
	}
}

func TestAdvancePhaseForward(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseSignup, 0)
	v, err := NewService(f).AdvancePhase(context.Background(), AdvanceParams{
		Slug: testSlug, To: PhaseSignupClosed, ActorUserID: testActor, Reason: "報名截止",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Tournament.Phase != PhaseSignupClosed {
		t.Errorf("回傳的階段 = %s,要 %s", v.Tournament.Phase, PhaseSignupClosed)
	}
	if len(f.updatePhaseCalls) != 1 {
		t.Fatalf("應呼叫 UpdatePhase 一次,得到 %d", len(f.updatePhaseCalls))
	}
	// From 必須帶上讀到的階段 —— 這是樂觀鎖的全部,漏了就變成無條件覆寫。
	got := f.updatePhaseCalls[0]
	if got.From != PhaseSignup || got.To != PhaseSignupClosed {
		t.Errorf("UpdatePhase 參數錯:%+v", got)
	}
	if got.ActorUserID != testActor {
		t.Errorf("稽核用的 actor 沒帶進去:%+v", got)
	}
}

// TestAdvancePhaseRejectsIllegal 跳階與倒退在 service 這層就要被擋住,
// 不能指望 repo 或資料庫。
func TestAdvancePhaseRejectsIllegal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		from Phase
		to   Phase
	}{
		{PhaseSignup, PhaseRanking},      // 跳一階
		{PhaseSignup, PhaseFinished},     // 直接跳到底
		{PhaseRanked, PhaseRanking},      // 倒退
		{PhaseInProgress, PhaseRanked},   // 開打後想改段位
		{PhaseFinished, PhaseInProgress}, // 結束後倒退
		{PhaseDrawing, PhaseDrawing},     // 原地不動
	}
	for _, c := range cases {
		f := newFake(c.from, 4)
		_, err := NewService(f).AdvancePhase(context.Background(), AdvanceParams{
			Slug: testSlug, To: c.to, ActorUserID: testActor,
		})
		if !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("%s → %s 要回 ErrIllegalTransition,得到 %v", c.from, c.to, err)
		}
		if len(f.updatePhaseCalls)+len(f.rollbackCalls) != 0 {
			t.Errorf("%s → %s 被擋下時不該碰資料庫", c.from, c.to)
		}
	}
}

// TestAdvancePhaseToDrawingRequiresAllRanked 未評段者不得進入抽籤。
//
// 這條是 schemas/20 的硬規則:未評段算不出 BP,硬推進去的結果是
// 那幾場悄悄變成沒有讓武,而對戰表上完全看不出來。
func TestAdvancePhaseToDrawingRequiresAllRanked(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseRanked, 4)
	f.unranked = 2
	_, err := NewService(f).AdvancePhase(context.Background(), AdvanceParams{
		Slug: testSlug, To: PhaseDrawing, ActorUserID: testActor,
	})
	if !errors.Is(err, ErrPlayersUnranked) {
		t.Fatalf("要回 ErrPlayersUnranked,得到 %v", err)
	}
	if len(f.updatePhaseCalls) != 0 {
		t.Error("被擋下時不該寫入階段")
	}
}

// TestAdvancePhaseRollbackClearsDraw 退回 ranked 要走 RollbackToRanked,
// 而不是普通的 UpdatePhase —— 對戰表與籤位必須在同一個 tx 裡一起作廢。
func TestAdvancePhaseRollbackClearsDraw(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseDrawing, 4)
	for i := range f.players {
		f.players[i].SeedNo = int32(i + 1)
	}
	v, err := NewService(f).AdvancePhase(context.Background(), AdvanceParams{
		Slug: testSlug, To: PhaseRanked, ActorUserID: testActor, Reason: "選手對段位有異議",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Tournament.Phase != PhaseRanked {
		t.Errorf("階段 = %s,要 %s", v.Tournament.Phase, PhaseRanked)
	}
	if len(f.rollbackCalls) != 1 {
		t.Fatalf("應走 RollbackToRanked,得到 %d 次呼叫", len(f.rollbackCalls))
	}
	if len(f.updatePhaseCalls) != 0 {
		t.Error("退回不該走普通的 UpdatePhase(那樣舊對戰表會留著)")
	}
	for _, p := range f.players {
		if p.SeedNo != 0 {
			t.Errorf("退回之後籤位要清空,%s 還是 %d", p.PublicID, p.SeedNo)
		}
	}
}

// TestAdvancePhaseConflictPropagates 樂觀鎖失敗要原樣上拋,不能吞掉。
//
// 兩個裁判同時按「進入下一階段」時只有一個會成功,輸的那個必須知道
// 自己什麼都沒做到 —— 回成功會讓他以為賽事被自己推進了。
func TestAdvancePhaseConflictPropagates(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseSignup, 0)
	f.updatePhaseErr = ErrPhaseConflict
	_, err := NewService(f).AdvancePhase(context.Background(), AdvanceParams{
		Slug: testSlug, To: PhaseSignupClosed, ActorUserID: testActor,
	})
	if !errors.Is(err, ErrPhaseConflict) {
		t.Errorf("要回 ErrPhaseConflict,得到 %v", err)
	}
}

// TestDestructiveOpsRequireActor 所有破壞性操作都要指名裁判,
// 否則 admin_audit_logs 記不出「是誰做的」。
func TestDestructiveOpsRequireActor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewService(newFake(PhaseDrawing, 4))

	if _, err := s.AdvancePhase(ctx, AdvanceParams{Slug: testSlug, To: PhaseInProgress}); !errors.Is(err, ErrActorRequired) {
		t.Errorf("AdvancePhase 要回 ErrActorRequired,得到 %v", err)
	}
	if _, err := s.Draw(ctx, DrawParams{Slug: testSlug}); !errors.Is(err, ErrActorRequired) {
		t.Errorf("Draw 要回 ErrActorRequired,得到 %v", err)
	}
	if _, _, err := s.SwapSeeds(ctx, SwapParams{Slug: testSlug}); !errors.Is(err, ErrActorRequired) {
		t.Errorf("SwapSeeds 要回 ErrActorRequired,得到 %v", err)
	}
	if _, err := s.SetRank(ctx, SetRankParams{Slug: testSlug, Rank: bp.RankWuwo}); !errors.Is(err, ErrActorRequired) {
		t.Errorf("SetRank 要回 ErrActorRequired,得到 %v", err)
	}
}

func TestSetRankInEditablePhases(t *testing.T) {
	t.Parallel()

	for _, phase := range []Phase{PhaseRanking, PhaseRanked} {
		f := newFake(phase, 4)
		p, err := NewService(f).SetRank(context.Background(), SetRankParams{
			Slug: testSlug, PlayerPublicID: "P01", Rank: bp.RankWuwo,
			ActorUserID: testActor, Reason: "實戰表現遠超自評",
		})
		if err != nil {
			t.Fatalf("%s 階段應可改段位:%v", phase, err)
		}
		if p.Rank != bp.RankWuwo {
			t.Errorf("段位 = %s,要 %s", p.Rank, bp.RankWuwo)
		}
		if len(f.setRankCalls) != 1 || f.setRankCalls[0].ActorUserID != testActor {
			t.Errorf("評段的裁判要寫進 ranked_by:%+v", f.setRankCalls)
		}
	}
}

// TestSetRankLockedAfterDrawing 抽籤之後改段位要回 ErrRanksLocked,
// 而且錯誤要與「還沒到評段階段」分得出來 —— 前者有明確的補救動作。
func TestSetRankLockedAfterDrawing(t *testing.T) {
	t.Parallel()

	for _, phase := range []Phase{PhaseDrawing, PhaseInProgress, PhaseFinished} {
		f := newFake(phase, 4)
		_, err := NewService(f).SetRank(context.Background(), SetRankParams{
			Slug: testSlug, PlayerPublicID: "P01", Rank: bp.RankWuwo, ActorUserID: testActor,
		})
		if !errors.Is(err, ErrRanksLocked) {
			t.Errorf("%s 階段要回 ErrRanksLocked,得到 %v", phase, err)
		}
		if len(f.setRankCalls) != 0 {
			t.Errorf("%s 階段不該寫入段位", phase)
		}
	}
}

func TestSetRankBeforeRankingPhase(t *testing.T) {
	t.Parallel()

	for _, phase := range []Phase{PhaseSignup, PhaseSignupClosed} {
		f := newFake(phase, 4)
		_, err := NewService(f).SetRank(context.Background(), SetRankParams{
			Slug: testSlug, PlayerPublicID: "P01", Rank: bp.RankWuwo, ActorUserID: testActor,
		})
		if !errors.Is(err, ErrWrongPhase) {
			t.Errorf("%s 階段要回 ErrWrongPhase,得到 %v", phase, err)
		}
		if errors.Is(err, ErrRanksLocked) {
			t.Errorf("%s 階段不是「鎖定」而是「還沒到時候」,兩者要分得出來", phase)
		}
	}
}

func TestSetRankRejectsInvalidRank(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseRanking, 4)
	for _, r := range []bp.Rank{bp.RankUnspecified, bp.Rank(5), bp.Rank(-1)} {
		_, err := NewService(f).SetRank(context.Background(), SetRankParams{
			Slug: testSlug, PlayerPublicID: "P01", Rank: r, ActorUserID: testActor,
		})
		if !errors.Is(err, ErrInvalidRank) {
			t.Errorf("段位 %d 要回 ErrInvalidRank,得到 %v", r, err)
		}
	}
}

func TestSetRankPlayerNotFound(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseRanking, 4)
	_, err := NewService(f).SetRank(context.Background(), SetRankParams{
		Slug: testSlug, PlayerPublicID: "不存在", Rank: bp.RankWuwo, ActorUserID: testActor,
	})
	if !errors.Is(err, ErrPlayerNotFound) {
		t.Errorf("要回 ErrPlayerNotFound,得到 %v", err)
	}
}

// TestDrawWritesSeed 抽籤必須把使用的種子交給 repo 寫進稽核紀錄。
//
// 這是整個抽籤流程唯一的可驗證性來源:裁判被質疑「這籤是不是喬過的」時,
// 拿種子重跑一次就能證明。種子沒寫下來的話,只能請人相信。
func TestDrawWritesSeed(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseDrawing, 8)
	res, err := NewService(f).Draw(context.Background(), DrawParams{
		Slug: testSlug, ActorUserID: testActor, Reason: "首次抽籤",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.drawCalls) != 1 {
		t.Fatalf("應呼叫 ReplaceDraw 一次,得到 %d", len(f.drawCalls))
	}
	call := f.drawCalls[0]
	if call.Seed == "" {
		t.Fatal("種子沒有被寫下來")
	}
	if call.Seed != res.Seed.String() {
		t.Errorf("回傳的種子(%s)與寫入的(%s)不一致", res.Seed, call.Seed)
	}
	if len(call.Seed) != 16 {
		t.Errorf("種子應為固定 16 位 hex,得到 %q", call.Seed)
	}
	if call.ActorUserID != testActor || call.Reason != "首次抽籤" {
		t.Errorf("稽核欄位沒帶進去:%+v", call)
	}
}

// TestDrawIsReproducibleFromSeed 同一個種子 + 同一份名單 = 位元相同的對戰表。
// 這條性質不成立的話,記錄種子就沒有任何意義。
func TestDrawIsReproducibleFromSeed(t *testing.T) {
	t.Parallel()

	first, err := NewService(newFake(PhaseDrawing, 11)).Draw(context.Background(), DrawParams{
		Slug: testSlug, ActorUserID: testActor,
	})
	if err != nil {
		t.Fatal(err)
	}

	second, err := NewService(newFake(PhaseDrawing, 11)).Draw(context.Background(), DrawParams{
		Slug: testSlug, ActorUserID: testActor, Seed: first.Seed.String(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if second.Seed != first.Seed {
		t.Fatalf("種子不同:%s vs %s", first.Seed, second.Seed)
	}
	if len(first.Seats) != len(second.Seats) {
		t.Fatalf("籤位數不同:%d vs %d", len(first.Seats), len(second.Seats))
	}
	for i := range first.Seats {
		if first.Seats[i] != second.Seats[i] {
			t.Fatalf("籤位 %d 不同:%+v vs %+v", i, first.Seats[i], second.Seats[i])
		}
	}
	for i := range first.Matches {
		if first.Matches[i] != second.Matches[i] {
			t.Fatalf("場次 %d 不同:%+v vs %+v", i, first.Matches[i], second.Matches[i])
		}
	}
}

// TestDrawSeatsAreDenseAndSorted 籤位必須是 1..n 的連續整數且已排序。
//
// 不排序的話,同一個種子會因為 map 走訪順序而寫出順序不同的 Seats,
// 稽核比對就失去意義;有缺號則代表有人被漏掉或多算了一個位子。
func TestDrawSeatsAreDenseAndSorted(t *testing.T) {
	t.Parallel()

	const n = 13
	res, err := NewService(newFake(PhaseDrawing, n)).Draw(context.Background(), DrawParams{
		Slug: testSlug, ActorUserID: testActor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Seats) != n {
		t.Fatalf("籤位數 = %d,要 %d", len(res.Seats), n)
	}
	seen := make(map[int64]bool, n)
	for i, s := range res.Seats {
		if s.SeedNo != int32(i+1) {
			t.Fatalf("Seats[%d].SeedNo = %d,要 %d(必須是排序過的連續整數)", i, s.SeedNo, i+1)
		}
		if seen[s.PlayerID] {
			t.Fatalf("選手 %d 出現兩次", s.PlayerID)
		}
		seen[s.PlayerID] = true
	}
	// Players 要跟 Seats 對得起來,否則前端顯示的名字會錯位。
	if len(res.Players) != n {
		t.Fatalf("Players 數 = %d,要 %d", len(res.Players), n)
	}
	for i := range res.Players {
		if res.Players[i].ID != res.Seats[i].PlayerID || res.Players[i].SeedNo != res.Seats[i].SeedNo {
			t.Fatalf("Players[%d] 與 Seats[%d] 對不上:%+v vs %+v", i, i, res.Players[i], res.Seats[i])
		}
	}
}

// TestDrawMatchCount 單淘汰的數學性質:場次數恆為人數 − 1。
func TestDrawMatchCount(t *testing.T) {
	t.Parallel()

	for _, n := range []int{1, 2, 3, 5, 8, 11, 16, 17, 32} {
		res, err := NewService(newFake(PhaseDrawing, n)).Draw(context.Background(), DrawParams{
			Slug: testSlug, ActorUserID: testActor,
		})
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if len(res.Matches) != n-1 {
			t.Errorf("n=%d 的場次數 = %d,要 %d", n, len(res.Matches), n-1)
		}
	}
}

func TestDrawWrongPhase(t *testing.T) {
	t.Parallel()

	for _, phase := range Phases() {
		if phase == PhaseDrawing {
			continue
		}
		f := newFake(phase, 4)
		_, err := NewService(f).Draw(context.Background(), DrawParams{
			Slug: testSlug, ActorUserID: testActor,
		})
		if !errors.Is(err, ErrWrongPhase) {
			t.Errorf("%s 階段抽籤要回 ErrWrongPhase,得到 %v", phase, err)
		}
		if len(f.drawCalls) != 0 {
			t.Errorf("%s 階段不該寫入對戰表", phase)
		}
	}
}

func TestDrawRequiresAllRanked(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseDrawing, 8)
	f.unranked = 1
	_, err := NewService(f).Draw(context.Background(), DrawParams{Slug: testSlug, ActorUserID: testActor})
	if !errors.Is(err, ErrPlayersUnranked) {
		t.Errorf("要回 ErrPlayersUnranked,得到 %v", err)
	}
	if len(f.drawCalls) != 0 {
		t.Error("被擋下時不該寫入對戰表")
	}
}

func TestDrawNoPlayers(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseDrawing, 0)
	_, err := NewService(f).Draw(context.Background(), DrawParams{Slug: testSlug, ActorUserID: testActor})
	if !errors.Is(err, ErrNoDrawablePlayers) {
		t.Errorf("要回 ErrNoDrawablePlayers,得到 %v", err)
	}
}

func TestDrawBadSeed(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseDrawing, 4)
	for _, seed := range []string{"abc", "zzzzzzzzzzzzzzzz", "00000000000000000"} {
		_, err := NewService(f).Draw(context.Background(), DrawParams{
			Slug: testSlug, ActorUserID: testActor, Seed: seed,
		})
		if !errors.Is(err, ErrInvalidSeed) {
			t.Errorf("種子 %q 要回 ErrInvalidSeed,得到 %v", seed, err)
		}
	}
}

// TestDrawRedrawReplaces 重抽就是再呼叫一次,不需要另一個 API。
func TestDrawRedrawReplaces(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseDrawing, 8)
	s := NewService(f)
	ctx := context.Background()
	first, err := s.Draw(ctx, DrawParams{Slug: testSlug, ActorUserID: testActor})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Draw(ctx, DrawParams{Slug: testSlug, ActorUserID: testActor, Reason: "重抽"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.drawCalls) != 2 {
		t.Fatalf("重抽應是第二次 ReplaceDraw,得到 %d", len(f.drawCalls))
	}
	if first.Seed == second.Seed {
		t.Error("兩次抽籤用了同一個種子,隨機性有問題")
	}
}

func TestSwapSeeds(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseDrawing, 8)
	s := NewService(f)
	ctx := context.Background()
	if _, err := s.Draw(ctx, DrawParams{Slug: testSlug, ActorUserID: testActor}); err != nil {
		t.Fatal(err)
	}

	beforeA := f.players[0].SeedNo
	beforeB := f.players[1].SeedNo
	a, b, err := s.SwapSeeds(ctx, SwapParams{
		Slug: testSlug, PlayerAPublicID: "P01", PlayerBPublicID: "P02",
		ActorUserID: testActor, Reason: "同公會首輪碰頭",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.SeedNo != beforeB || b.SeedNo != beforeA {
		t.Errorf("回傳的籤位沒換過來:a=%d b=%d(原本 %d / %d)", a.SeedNo, b.SeedNo, beforeA, beforeB)
	}
	if len(f.swapCalls) != 1 {
		t.Fatalf("應呼叫 SwapSeeds 一次,得到 %d", len(f.swapCalls))
	}
	if f.swapCalls[0].ActorUserID != testActor {
		t.Errorf("稽核用的 actor 沒帶進去:%+v", f.swapCalls[0])
	}
}

func TestSwapSeedsWrongPhase(t *testing.T) {
	t.Parallel()

	for _, phase := range Phases() {
		if phase == PhaseDrawing {
			continue
		}
		f := newFake(phase, 4)
		for i := range f.players {
			f.players[i].SeedNo = int32(i + 1)
		}
		_, _, err := NewService(f).SwapSeeds(context.Background(), SwapParams{
			Slug: testSlug, PlayerAPublicID: "P01", PlayerBPublicID: "P02", ActorUserID: testActor,
		})
		if !errors.Is(err, ErrWrongPhase) {
			t.Errorf("%s 階段要回 ErrWrongPhase,得到 %v", phase, err)
		}
	}
}

func TestSwapSeedsSamePlayer(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseDrawing, 4)
	for i := range f.players {
		f.players[i].SeedNo = int32(i + 1)
	}
	_, _, err := NewService(f).SwapSeeds(context.Background(), SwapParams{
		Slug: testSlug, PlayerAPublicID: "P01", PlayerBPublicID: "P01", ActorUserID: testActor,
	})
	if !errors.Is(err, ErrSamePlayer) {
		t.Errorf("要回 ErrSamePlayer,得到 %v", err)
	}
	if len(f.swapCalls) != 0 {
		t.Error("不該寫入")
	}
}

// TestSwapSeedsUnseeded 沒有籤位的人不能交換 ——
// 那會變成把對方的籤位讓出去,留下一張少一個人的對戰表。
func TestSwapSeedsUnseeded(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseDrawing, 4)
	f.players[0].SeedNo = 1 // 只有 P01 有籤位
	_, _, err := NewService(f).SwapSeeds(context.Background(), SwapParams{
		Slug: testSlug, PlayerAPublicID: "P01", PlayerBPublicID: "P02", ActorUserID: testActor,
	})
	if !errors.Is(err, ErrPlayerNotSeeded) {
		t.Errorf("要回 ErrPlayerNotSeeded,得到 %v", err)
	}
	if len(f.swapCalls) != 0 {
		t.Error("不該寫入")
	}
}

func TestSwapSeedsPlayerNotFound(t *testing.T) {
	t.Parallel()

	f := newFake(PhaseDrawing, 4)
	for i := range f.players {
		f.players[i].SeedNo = int32(i + 1)
	}
	_, _, err := NewService(f).SwapSeeds(context.Background(), SwapParams{
		Slug: testSlug, PlayerAPublicID: "P01", PlayerBPublicID: "不存在", ActorUserID: testActor,
	})
	if !errors.Is(err, ErrPlayerNotFound) {
		t.Errorf("要回 ErrPlayerNotFound,得到 %v", err)
	}
}

// TestDrawSeedRoundTrip 種子的字串形式要能原樣讀回來。
func TestDrawSeedRoundTrip(t *testing.T) {
	t.Parallel()

	for _, v := range []DrawSeed{0, 1, 0x0f, 1 << 63, ^DrawSeed(0)} {
		s := v.String()
		if len(s) != 16 {
			t.Errorf("%d 的字串形式長度 = %d,要 16(固定寬度)", v, len(s))
		}
		got, err := ParseDrawSeed(s)
		if err != nil {
			t.Fatalf("ParseDrawSeed(%q): %v", s, err)
		}
		if got != v {
			t.Errorf("來回轉換後 %d != %d", got, v)
		}
	}
}

func TestNewDrawSeedIsRandom(t *testing.T) {
	t.Parallel()

	seen := make(map[DrawSeed]bool, 64)
	for range 64 {
		s, err := NewDrawSeed()
		if err != nil {
			t.Fatal(err)
		}
		if seen[s] {
			t.Fatalf("64 次之內就撞了同一個種子 %s,亂數來源有問題", s)
		}
		seen[s] = true
	}
}
