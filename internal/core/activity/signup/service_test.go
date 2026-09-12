package signup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/danicotech/hestia/internal/core/activity/bp"
	"github.com/danicotech/hestia/internal/core/activity/tournament"
)

const (
	testSlug  = "2026-baiye-shifeng"
	testActor = int64(77)
)

// regRow 是 fakeRepo 裡的一列報名(tournament_players 的記憶體版本)。
//
// **刻意不放 passcode_hash 與 passcode_issued_at**:這一層沒有任何東西讀它們
// (登入只要遊戲ID),放進來就是一份永遠不會被檢查的假資料。那兩欄真的被寫對了
// 沒有,由 signuppg 的測試直接對 Postgres 驗。
type regRow struct {
	player tournament.Player
}

// fakeRepo 是 Repo 的記憶體實作,行為刻意貼著 adapter 被要求做的事:
// 依 game_id 找或建 fencer、撞 UNIQUE 就回 ErrAlreadyRegistered、
// 綁定時同時寫兩處。這樣測到的才是 service 真正會遇到的介面語意。
type fakeRepo struct {
	tourn   tournament.Tournament
	fencers map[string]*Fencer
	rows    []*regRow
	nextID  int64

	createCalls []CreateRegistrationParams
	updateCalls []UpdatePasscodeParams
	bindCalls   []BindParams
	// lookups 數 PlayerByGameID 被叫了幾次:格式不合的登入不該打到資料庫。
	lookups int
}

func newFakeRepo(phase tournament.Phase) *fakeRepo {
	return &fakeRepo{
		tourn: tournament.Tournament{
			ID: 1, PublicID: "T1", Slug: testSlug, Name: "百業試鋒",
			CommunityID: 1, Phase: phase, ConfigRaw: []byte(`{}`),
		},
		fencers: map[string]*Fencer{},
	}
}

func (f *fakeRepo) TournamentBySlug(_ context.Context, slug string) (tournament.Tournament, error) {
	if slug != f.tourn.Slug {
		return tournament.Tournament{}, tournament.ErrTournamentNotFound
	}
	return f.tourn, nil
}

func (f *fakeRepo) CreateRegistration(_ context.Context, p CreateRegistrationParams) (Registration, error) {
	f.createCalls = append(f.createCalls, p)
	// 對應 adapter 那句 WHERE phase = $n:service 讀階段與寫入之間有時間差。
	if p.RequirePhase != "" && f.tourn.Phase != p.RequirePhase {
		return Registration{}, tournament.ErrWrongPhase
	}

	fencer, returning := f.fencers[p.GameID]
	previous := bp.RankUnspecified
	if returning {
		previous = fencer.LastRankLevel
	} else {
		f.nextID++
		fencer = &Fencer{
			ID:       f.nextID,
			PublicID: fmt.Sprintf("F%02d", f.nextID),
			GameID:   p.GameID,
		}
		f.fencers[p.GameID] = fencer
	}

	// UNIQUE (tournament_id, fencer_id):一人一屆只能報一次。
	for _, r := range f.rows {
		if r.player.FencerID == fencer.ID && r.player.TournamentID == p.TournamentID {
			return Registration{}, ErrAlreadyRegistered
		}
	}

	f.nextID++
	row := &regRow{
		player: tournament.Player{
			ID:               f.nextID,
			PublicID:         fmt.Sprintf("P%02d", f.nextID),
			TournamentID:     p.TournamentID,
			FencerID:         fencer.ID,
			GameID:           p.GameID,
			DisplayName:      p.DisplayName,
			DiscordName:      p.DiscordName,
			SelfRatedRank:    p.SelfRatedRank,
			LadderRank:       p.LadderRank,
			LadderScore:      p.LadderScore,
			ArtsNote:         p.ArtsNote,
			AvailabilityNote: p.AvailabilityNote,
			Status:           tournament.PlayerActive,
		},
	}
	f.rows = append(f.rows, row)

	fencer.DiscordName = p.DiscordName
	fencer.TournamentsPlayed++

	return Registration{
		Player:          row.player,
		Fencer:          *fencer,
		ReturningFencer: returning,
		PreviousRank:    previous,
	}, nil
}

// PlayerByGameID 刻意**不看 status** —— adapter 也不看(誰登得進來由 Service 決定)。
func (f *fakeRepo) PlayerByGameID(_ context.Context, tournamentID int64, gameID string) (tournament.Player, error) {
	f.lookups++
	for _, r := range f.rows {
		if r.player.TournamentID == tournamentID && r.player.GameID == gameID {
			return r.player, nil
		}
	}
	return tournament.Player{}, tournament.ErrPlayerNotFound
}

// setStatus 讓測試模擬裁判把某人標成棄賽 / 淘汰。
func (f *fakeRepo) setStatus(gameID string, status tournament.PlayerStatus) {
	for _, r := range f.rows {
		if r.player.GameID == gameID {
			r.player.Status = status
		}
	}
}

func (f *fakeRepo) PlayerByPublicID(_ context.Context, tournamentID int64, publicID string) (tournament.Player, error) {
	for _, r := range f.rows {
		if r.player.TournamentID == tournamentID && r.player.PublicID == publicID {
			return r.player, nil
		}
	}
	return tournament.Player{}, tournament.ErrPlayerNotFound
}

// UpdatePasscode 只記下這次呼叫:換發的效果(雜湊被換掉、issued_at 前進)
// 全部發生在資料庫那一層,而這裡沒有讀者 —— 見 regRow 的註解。
func (f *fakeRepo) UpdatePasscode(_ context.Context, p UpdatePasscodeParams) error {
	f.updateCalls = append(f.updateCalls, p)
	for _, r := range f.rows {
		if r.player.ID == p.PlayerID {
			return nil
		}
	}
	return tournament.ErrPlayerNotFound
}

func (f *fakeRepo) BindPlatformAccount(_ context.Context, p BindParams) (tournament.Player, error) {
	f.bindCalls = append(f.bindCalls, p)
	// fencers_user_id_uq:一個平台帳號只能是一個人。
	for _, fe := range f.fencers {
		if fe.UserID != nil && *fe.UserID == p.UserID && fe.ID != p.FencerID {
			return tournament.Player{}, ErrUserAlreadyBound
		}
	}
	for _, r := range f.rows {
		if r.player.ID != p.PlayerID {
			continue
		}
		uid := p.UserID
		r.player.UserID = &uid
		for _, fe := range f.fencers {
			if fe.ID == p.FencerID {
				fe.UserID = &uid
			}
		}
		return r.player, nil
	}
	return tournament.Player{}, tournament.ErrPlayerNotFound
}

// fencerByGameID 是測試的輔助讀取,不是 port 的一部分。
func (f *fakeRepo) fencerByGameID(gameID string) *Fencer { return f.fencers[gameID] }

func newTestService(t *testing.T, phase tournament.Phase) (*Service, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo(phase)
	return NewService(repo, newTestHasher(t)), repo
}

func validRegistration(gameID string) RegisterParams {
	return RegisterParams{
		TournamentSlug:   testSlug,
		GameID:           gameID,
		DisplayName:      "御風羽",
		DiscordName:      "yufengyu#0001",
		SelfRatedRank:    bp.RankDuanshui,
		LadderRank:       "論劍·地榜",
		LadderScore:      1820,
		ArtsNote:         "常用太虛劍意",
		AvailabilityNote: "平日晚上",
	}
}

// TestRegisterStoresHashButReturnsNoPasscode:通行碼照樣產生並存進去
// (passcode_hash 是 NOT NULL,而 passcode_issued_at 是 session 作廢的依據),
// 但**不回給選手** —— 登入用不到它,吐一組沒用的密碼只會讓人以為要保存。
func TestRegisterStoresHashButReturnsNoPasscode(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	res, err := s.Register(context.Background(), validRegistration("御風羽"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ReturningFencer {
		t.Error("初次報名不該標成回鍋選手")
	}
	if res.PreviousRank != bp.RankUnspecified {
		t.Errorf("初次報名的往屆段位應為未評定,得到 %s", res.PreviousRank)
	}

	if len(repo.createCalls) != 1 {
		t.Fatalf("應寫入一次,得到 %d", len(repo.createCalls))
	}
	stored := repo.createCalls[0].PasscodeHash
	if stored == "" {
		t.Fatal("passcode_hash 是 NOT NULL,報名仍必須存一份雜湊")
	}
	// 存的必須是雜湊而不是明碼:雜湊字串本身看得出演算法標籤。
	if !strings.HasPrefix(stored, "pbkdf2-sha256$") {
		t.Fatalf("存進去的不像雜湊:%q", stored)
	}

	// 結構裡根本沒有 Passcode 欄位,所以「回應含通行碼」在編譯期就不可能。
	// 這裡再從序列化的角度確認一次:整個結果裡不該出現任何通行碼字元組合。
	if strings.Contains(fmt.Sprintf("%+v", *res), stored) {
		t.Error("報名結果不該帶出任何通行碼相關的東西")
	}
}

// TestRegisterPassesPhaseGuardToRepo 寫入時必須把階段條件一起帶下去。
//
// service 讀階段與寫入之間有時間差,裁判剛好在那一瞬封閉報名的話,
// 少了這個條件就會漏進一筆 —— 而報名截止後混進一個人,對已抽好的籤表是災難。
func TestRegisterPassesPhaseGuardToRepo(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	if _, err := s.Register(context.Background(), validRegistration("御風羽")); err != nil {
		t.Fatal(err)
	}
	if repo.createCalls[0].RequirePhase != tournament.PhaseSignup {
		t.Errorf("RequirePhase = %q,要 %q", repo.createCalls[0].RequirePhase, tournament.PhaseSignup)
	}
}

// TestRegisterOnlyInSignupPhase 報名只在 signup 階段開放。
func TestRegisterOnlyInSignupPhase(t *testing.T) {
	t.Parallel()

	for _, phase := range tournament.Phases() {
		if phase == tournament.PhaseSignup {
			continue
		}
		s, repo := newTestService(t, phase)
		_, err := s.Register(context.Background(), validRegistration("御風羽"))
		if !errors.Is(err, tournament.ErrWrongPhase) {
			t.Errorf("%s 階段報名要回 ErrWrongPhase,得到 %v", phase, err)
		}
		if len(repo.createCalls) != 0 {
			t.Errorf("%s 階段不該寫入", phase)
		}
	}
}

// TestRegisterReturningFencerLinksHistory 回鍋選手要接上既有檔案並說出上屆段位。
//
// 這是 fencers 這張表存在的全部理由(schemas/26):
// 「這個人是誰」跨越所有屆別,而 tournament_players 是逐屆快照。
func TestRegisterReturningFencerLinksHistory(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	ctx := context.Background()

	first, err := s.Register(ctx, validRegistration("御風羽"))
	if err != nil {
		t.Fatal(err)
	}

	// 上一屆結束:裁判評了段,檔案留下 last_rank_level。
	fencer := repo.fencerByGameID("御風羽")
	fencer.LastRankLevel = bp.RankDuanshui
	// 下一屆:同一個 tournament_id 會被 UNIQUE 擋,所以換一屆。
	repo.tourn.ID = 2

	second, err := s.Register(ctx, validRegistration("御風羽"))
	if err != nil {
		t.Fatal(err)
	}

	if !second.ReturningFencer {
		t.Error("同一個遊戲ID 再次報名要標成回鍋選手")
	}
	if second.PreviousRank != bp.RankDuanshui {
		t.Errorf("往屆段位 = %s,要 %s", second.PreviousRank, bp.RankDuanshui)
	}
	if second.Player.FencerID != first.Player.FencerID {
		t.Errorf("回鍋選手要接上同一份檔案:%d vs %d", second.Player.FencerID, first.Player.FencerID)
	}
	if second.Player.ID == first.Player.ID {
		t.Error("逐屆的 tournament_players 必須是不同列(本屆段位不能被下屆覆寫)")
	}
	// 本屆段位要重新評,不能沿用上屆。
	if second.Player.Rank != bp.RankUnspecified {
		t.Errorf("本屆段位應為未評定,得到 %s", second.Player.Rank)
	}
	if fencer.TournamentsPlayed != 2 {
		t.Errorf("參賽屆數 = %d,要 2", fencer.TournamentsPlayed)
	}
	// 兩屆的通行碼雜湊是各自發的,不該共用(逐屆一份,換發也只影響一屆)。
	if repo.createCalls[0].PasscodeHash == repo.createCalls[1].PasscodeHash {
		t.Error("兩屆的通行碼雜湊不該相同")
	}
}

// TestRegisterDuplicateBlockedByUniqueConstraint 連點報名鈕由 UNIQUE 擋,
// 不加冪等鍵(一個概念一個權威,鐵則 9)。
func TestRegisterDuplicateBlockedByUniqueConstraint(t *testing.T) {
	t.Parallel()

	s, _ := newTestService(t, tournament.PhaseSignup)
	ctx := context.Background()
	if _, err := s.Register(ctx, validRegistration("御風羽")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Register(ctx, validRegistration("御風羽"))
	if !errors.Is(err, ErrAlreadyRegistered) {
		t.Errorf("重複報名要回 ErrAlreadyRegistered,得到 %v", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*RegisterParams)
		want   error
	}{
		{"遊戲ID 空白", func(p *RegisterParams) { p.GameID = "   " }, ErrGameIDRequired},
		{"遊戲ID 含控制字元", func(p *RegisterParams) { p.GameID = "御風" + string(rune(0)) + "羽" }, ErrInvalidGameID},
		{"遊戲ID 過長", func(p *RegisterParams) { p.GameID = strings.Repeat("字", 65) }, ErrInvalidGameID},
		{"自評段位超出範圍", func(p *RegisterParams) { p.SelfRatedRank = bp.Rank(9) }, tournament.ErrInvalidRank},
		{"武學描述過長", func(p *RegisterParams) { p.ArtsNote = strings.Repeat("字", 2001) }, ErrFieldTooLong},
	}
	for _, c := range cases {
		s, repo := newTestService(t, tournament.PhaseSignup)
		params := validRegistration("御風羽")
		c.mutate(&params)
		_, err := s.Register(context.Background(), params)
		if !errors.Is(err, c.want) {
			t.Errorf("%s:要回 %v,得到 %v", c.name, c.want, err)
		}
		if len(repo.createCalls) != 0 {
			t.Errorf("%s:驗證不過時不該寫入", c.name)
		}
	}
}

// TestRegisterTrimsAndDefaults 遊戲ID 的前後空白必須在進 DB 前清掉。
//
// 沒清的話,「御風羽」與「御風羽 」是 UNIQUE (game_id) 眼中的兩個人,
// 而那兩列在畫面上長得一模一樣。
func TestRegisterTrimsAndDefaults(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	params := validRegistration("  御風羽  ")
	params.DisplayName = "   " // 留空要沿用 game_id
	if _, err := s.Register(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	got := repo.createCalls[0]
	if got.GameID != "御風羽" {
		t.Errorf("遊戲ID 沒去空白:%q", got.GameID)
	}
	if got.DisplayName != "御風羽" {
		t.Errorf("顯示名留空時要沿用遊戲ID,得到 %q", got.DisplayName)
	}
}

// TestRegisterNeedsOnlyGameID 是這次決策的正面:報名表只有遊戲ID 必填,
// 其餘每一欄留空都要能報成功 —— 多一個必填欄位就在報名頁攔掉一部分人。
func TestRegisterNeedsOnlyGameID(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	res, err := s.Register(context.Background(), RegisterParams{
		TournamentSlug: testSlug,
		GameID:         "御風羽",
	})
	if err != nil {
		t.Fatalf("只填遊戲ID 應該報得成:%v", err)
	}
	got := repo.createCalls[0]
	if got.DiscordName != "" {
		t.Errorf("Discord 名稱留空要原樣寫入,得到 %q", got.DiscordName)
	}
	if got.DisplayName != "御風羽" {
		t.Errorf("顯示名留空要沿用遊戲ID,得到 %q", got.DisplayName)
	}
	if got.SelfRatedRank != bp.RankUnspecified || got.LadderRank != "" ||
		got.LadderScore != 0 || got.ArtsNote != "" || got.AvailabilityNote != "" {
		t.Errorf("選填欄位留空不該被填上任何東西:%+v", got)
	}
	// 選填欄位留空的人照樣是一位正常的參賽者。
	if res.Player.Status != tournament.PlayerActive {
		t.Errorf("狀態 = %s,要 active", res.Player.Status)
	}
	if res.Player.PublicID == "" {
		t.Error("報名結果要帶回選手的 public_id")
	}
}

// TestLoginSucceedsWithGameIDAlone 登入只要遊戲ID(2026-09-13 定案)。
func TestLoginSucceedsWithGameIDAlone(t *testing.T) {
	t.Parallel()

	s, _ := newTestService(t, tournament.PhaseSignup)
	ctx := context.Background()
	reg, err := s.Register(ctx, validRegistration("御風羽"))
	if err != nil {
		t.Fatal(err)
	}

	player, err := s.Login(ctx, LoginParams{TournamentSlug: testSlug, GameID: "御風羽"})
	if err != nil {
		t.Fatal(err)
	}
	if player.PublicID != reg.Player.PublicID {
		t.Errorf("登入回傳的選手不對:%s vs %s", player.PublicID, reg.Player.PublicID)
	}
}

// TestLoginTrimsGameID 前後空白要與報名時同一套整理,否則「貼上時多一個空白」
// 的人會被擋在門外,而畫面上那兩個字串長得一模一樣。
func TestLoginTrimsGameID(t *testing.T) {
	t.Parallel()

	s, _ := newTestService(t, tournament.PhaseSignup)
	ctx := context.Background()
	if _, err := s.Register(ctx, validRegistration("御風羽")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, LoginParams{TournamentSlug: testSlug, GameID: "  御風羽 "}); err != nil {
		t.Errorf("前後空白應該被清掉:%v", err)
	}
}

// TestLoginRejectsNonActivePlayers 是這次決策**唯一的補救**,所以是這個套件
// 最重要的一條測試:沒有密碼可以換,把一個人擋在外面的唯一槓桿就是他的狀態。
//
// 而且失敗的樣子必須與「查無此人」一模一樣 —— 分得出來的話,任何人都能問出
// 「某某是不是被裁判棄賽了」,那不該由一個匿名請求答得出來。
func TestLoginRejectsNonActivePlayers(t *testing.T) {
	t.Parallel()

	for _, status := range []tournament.PlayerStatus{tournament.PlayerWithdrawn, tournament.PlayerEliminated} {
		s, repo := newTestService(t, tournament.PhaseSignup)
		ctx := context.Background()
		if _, err := s.Register(ctx, validRegistration("御風羽")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Login(ctx, LoginParams{TournamentSlug: testSlug, GameID: "御風羽"}); err != nil {
			t.Fatalf("%s:標記之前應該登得進來,%v", status, err)
		}

		repo.setStatus("御風羽", status)

		_, blocked := s.Login(ctx, LoginParams{TournamentSlug: testSlug, GameID: "御風羽"})
		if !errors.Is(blocked, ErrInvalidCredentials) {
			t.Fatalf("%s 要回 ErrInvalidCredentials,得到 %v", status, blocked)
		}
		_, unknown := s.Login(ctx, LoginParams{TournamentSlug: testSlug, GameID: "查無此人"})
		if blocked.Error() != unknown.Error() {
			t.Errorf("%s 與查無此人的錯誤必須逐字相同:\n  %q\n  %q",
				status, blocked.Error(), unknown.Error())
		}
	}
}

// TestLoginFailuresAreIndistinguishable:查無、非 active、空白、格式不合
// 必須回**完全相同**的錯誤,連訊息都一樣。
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	ctx := context.Background()
	if _, err := s.Register(ctx, validRegistration("御風羽")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register(ctx, validRegistration("聽雪樓")); err != nil {
		t.Fatal(err)
	}
	repo.setStatus("聽雪樓", tournament.PlayerWithdrawn)

	_, withdrawn := s.Login(ctx, LoginParams{TournamentSlug: testSlug, GameID: "聽雪樓"})
	_, noSuchUser := s.Login(ctx, LoginParams{TournamentSlug: testSlug, GameID: "查無此人"})
	_, emptyUser := s.Login(ctx, LoginParams{TournamentSlug: testSlug, GameID: "   "})
	_, ctrl := s.Login(ctx, LoginParams{TournamentSlug: testSlug, GameID: "御風" + string(rune(0)) + "羽"})
	_, tooLong := s.Login(ctx, LoginParams{TournamentSlug: testSlug, GameID: strings.Repeat("字", 65)})

	all := map[string]error{
		"棄賽": withdrawn, "查無此人": noSuchUser, "遊戲ID 空白": emptyUser,
		"含控制字元": ctrl, "過長": tooLong,
	}
	for name, err := range all {
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("%s 要回 ErrInvalidCredentials,得到 %v", name, err)
		}
		if err.Error() != noSuchUser.Error() {
			t.Errorf("%s 的訊息與查無此人不同:\n  %q\n  %q", name, err.Error(), noSuchUser.Error())
		}
		// 格式錯誤特別容易漏:回 ErrInvalidGameID 等於告訴對方「這串字連查都不必查」。
		if errors.Is(err, ErrInvalidGameID) || errors.Is(err, ErrGameIDRequired) {
			t.Errorf("%s 洩漏了格式判斷:%v", name, err)
		}
	}
	// 訊息裡不得出現遊戲ID —— 把輸入回顯出來也是一種洩漏(而且是 XSS 的溫床)。
	if strings.Contains(noSuchUser.Error(), "查無此人") {
		t.Errorf("錯誤訊息不該回顯使用者輸入的遊戲ID:%q", noSuchUser.Error())
	}
}

// TestLoginDoesNotTouchRepoOnBadGameID:格式不合的輸入連查都不該查。
// 這既是上一條的實作面,也讓「送一串垃圾就叫我們打一次資料庫」不成立。
func TestLoginDoesNotTouchRepoOnBadGameID(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	if _, err := s.Login(context.Background(), LoginParams{
		TournamentSlug: testSlug, GameID: strings.Repeat("字", 65),
	}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("要回 ErrInvalidCredentials,得到 %v", err)
	}
	if repo.lookups != 0 {
		t.Errorf("格式不合不該打資料庫,查了 %d 次", repo.lookups)
	}
}

func TestLoginUnknownTournamentIsA404(t *testing.T) {
	t.Parallel()

	// 賽事 slug 在網址列上,不是秘密 —— 它查不到就是真的 404,
	// 沒有理由偽裝成登入失敗。
	s, _ := newTestService(t, tournament.PhaseSignup)
	_, err := s.Login(context.Background(), LoginParams{
		TournamentSlug: "不存在的賽事", GameID: "御風羽",
	})
	if !errors.Is(err, tournament.ErrTournamentNotFound) {
		t.Errorf("要回 ErrTournamentNotFound,得到 %v", err)
	}
}

// TestLoginWorksInEveryPhase 登入不限階段 —— 選手在任何時候都可能要看自己的場次。
func TestLoginWorksInEveryPhase(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	ctx := context.Background()
	if _, err := s.Register(ctx, validRegistration("御風羽")); err != nil {
		t.Fatal(err)
	}
	for _, phase := range tournament.Phases() {
		repo.tourn.Phase = phase
		if _, err := s.Login(ctx, LoginParams{
			TournamentSlug: testSlug, GameID: "御風羽",
		}); err != nil {
			t.Errorf("%s 階段應可登入:%v", phase, err)
		}
	}
}

// TestRegeneratePasscodeRotatesSecretWithoutLockingOut
//
// 換發現在的用途是**踢掉已發出的 session**(passcode_issued_at 一起前進,
// 那一半在 internal/core/activity/session 測)。這裡守住兩件事:
// 真的換掉了一個秘密,以及**沒有把人鎖在外面** —— 登入只要遊戲ID,
// 一個被踢下線的選手應該能立刻再登入,不然這個按鈕就變成了永久封鎖。
func TestRegeneratePasscodeRotatesSecretWithoutLockingOut(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	ctx := context.Background()
	reg, err := s.Register(ctx, validRegistration("御風羽"))
	if err != nil {
		t.Fatal(err)
	}
	oldHash := repo.createCalls[0].PasscodeHash

	fresh, err := s.RegeneratePasscode(ctx, RegenerateParams{
		TournamentSlug: testSlug, PlayerPublicID: reg.Player.PublicID,
		ActorUserID: testActor, Reason: "疑似有人冒用,先把他踢下線",
	})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Passcode == "" || len(fresh.Passcode) != PasscodeLength {
		t.Fatalf("裁判那張收據要是一組真的通行碼,得到 %q", fresh.Passcode)
	}
	if repo.updateCalls[0].PasscodeHash == oldHash {
		t.Error("雜湊沒換 —— 這次換發什麼也沒換掉")
	}

	// 換發不是封鎖:同一個遊戲ID 照樣登得進來。
	if _, err := s.Login(ctx, LoginParams{TournamentSlug: testSlug, GameID: "御風羽"}); err != nil {
		t.Errorf("換發之後仍應登得進來(要擋人請用棄賽):%v", err)
	}

	if len(repo.updateCalls) != 1 {
		t.Fatalf("應呼叫 UpdatePasscode 一次,得到 %d", len(repo.updateCalls))
	}
	call := repo.updateCalls[0]
	if call.ActorUserID != testActor || call.Reason == "" {
		t.Errorf("稽核欄位沒帶進去:%+v", call)
	}
	// 明碼與稽核紀錄之間必須沒有任何通道。
	if strings.Contains(call.PasscodeHash, fresh.Passcode) {
		t.Error("寫入的雜湊裡含有明碼")
	}
}

// TestRegeneratePasscodeRequiresActor 沒有選手自助重取這條路。
//
// 遊戲ID 是公開資訊(對戰表上就有),自助重取等於「輸入任何人的遊戲ID
// 就能把他的通行碼換掉」—— 一鍵帳號接管,而且原持有人只會以為自己抄錯了。
func TestRegeneratePasscodeRequiresActor(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	ctx := context.Background()
	reg, err := s.Register(ctx, validRegistration("御風羽"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.RegeneratePasscode(ctx, RegenerateParams{
		TournamentSlug: testSlug, PlayerPublicID: reg.Player.PublicID,
	})
	if !errors.Is(err, tournament.ErrActorRequired) {
		t.Errorf("要回 ErrActorRequired,得到 %v", err)
	}
	if len(repo.updateCalls) != 0 {
		t.Error("不該寫入")
	}
}

func TestRegeneratePasscodePlayerNotFound(t *testing.T) {
	t.Parallel()

	s, _ := newTestService(t, tournament.PhaseSignup)
	_, err := s.RegeneratePasscode(context.Background(), RegenerateParams{
		TournamentSlug: testSlug, PlayerPublicID: "不存在", ActorUserID: testActor,
	})
	if !errors.Is(err, tournament.ErrPlayerNotFound) {
		t.Errorf("要回 ErrPlayerNotFound,得到 %v", err)
	}
}

// TestBindPlatformAccountWritesBothTables 綁定要同時寫 fencers 與 tournament_players。
// 只寫一處的話,領獎時查哪一張表會得到不同答案。
func TestBindPlatformAccountWritesBothTables(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	ctx := context.Background()
	reg, err := s.Register(ctx, validRegistration("御風羽"))
	if err != nil {
		t.Fatal(err)
	}

	const userID = int64(555)
	player, err := s.BindPlatformAccount(ctx, BindAccountParams{
		TournamentSlug: testSlug, PlayerPublicID: reg.Player.PublicID, UserID: userID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !player.Bound() || *player.UserID != userID {
		t.Fatalf("選手沒綁上:%+v", player.UserID)
	}
	if len(repo.bindCalls) != 1 {
		t.Fatalf("應呼叫一次,得到 %d", len(repo.bindCalls))
	}
	// FencerID 必須帶下去,adapter 才寫得到 fencers.user_id。
	if repo.bindCalls[0].FencerID != reg.Player.FencerID {
		t.Errorf("FencerID 沒帶進去:%+v", repo.bindCalls[0])
	}
	fencer := repo.fencerByGameID("御風羽")
	if fencer.UserID == nil || *fencer.UserID != userID {
		t.Error("跨屆檔案 fencers.user_id 也要寫上,否則領獎查不到收款對象")
	}
}

// TestBindPlatformAccountIsIdempotent 重複綁同一個帳號視為成功。
// 使用者連點兩次不該看到錯誤,而這件事沒有副作用可言。
func TestBindPlatformAccountIsIdempotent(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	ctx := context.Background()
	reg, err := s.Register(ctx, validRegistration("御風羽"))
	if err != nil {
		t.Fatal(err)
	}
	params := BindAccountParams{TournamentSlug: testSlug, PlayerPublicID: reg.Player.PublicID, UserID: 555}
	if _, err := s.BindPlatformAccount(ctx, params); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindPlatformAccount(ctx, params); err != nil {
		t.Fatalf("重複綁定同一個帳號應視為成功:%v", err)
	}
	if len(repo.bindCalls) != 1 {
		t.Errorf("第二次不該再寫入,得到 %d 次", len(repo.bindCalls))
	}
}

// TestBindPlatformAccountRejectsRebind 改綁到別的帳號一律拒絕。
// 綁定決定獎金匯給誰,換收款人必須由裁判介入並留稽核紀錄。
func TestBindPlatformAccountRejectsRebind(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	ctx := context.Background()
	reg, err := s.Register(ctx, validRegistration("御風羽"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindPlatformAccount(ctx, BindAccountParams{
		TournamentSlug: testSlug, PlayerPublicID: reg.Player.PublicID, UserID: 555,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = s.BindPlatformAccount(ctx, BindAccountParams{
		TournamentSlug: testSlug, PlayerPublicID: reg.Player.PublicID, UserID: 999,
	})
	if !errors.Is(err, ErrAlreadyBound) {
		t.Errorf("要回 ErrAlreadyBound,得到 %v", err)
	}
	if len(repo.bindCalls) != 1 {
		t.Error("被拒絕時不該寫入")
	}
}

// TestBindPlatformAccountRejectsSharedUser 一個平台帳號只能是一個人
// (fencers_user_id_uq)。這條由資料庫擋,service 只負責把錯誤上拋。
func TestBindPlatformAccountRejectsSharedUser(t *testing.T) {
	t.Parallel()

	s, _ := newTestService(t, tournament.PhaseSignup)
	ctx := context.Background()
	a, err := s.Register(ctx, validRegistration("御風羽"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Register(ctx, validRegistration("聽雪樓"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindPlatformAccount(ctx, BindAccountParams{
		TournamentSlug: testSlug, PlayerPublicID: a.Player.PublicID, UserID: 555,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = s.BindPlatformAccount(ctx, BindAccountParams{
		TournamentSlug: testSlug, PlayerPublicID: b.Player.PublicID, UserID: 555,
	})
	if !errors.Is(err, ErrUserAlreadyBound) {
		t.Errorf("要回 ErrUserAlreadyBound,得到 %v", err)
	}
}

func TestBindPlatformAccountRequiresUser(t *testing.T) {
	t.Parallel()

	s, repo := newTestService(t, tournament.PhaseSignup)
	_, err := s.BindPlatformAccount(context.Background(), BindAccountParams{
		TournamentSlug: testSlug, PlayerPublicID: "P02",
	})
	if !errors.Is(err, ErrUserRequired) {
		t.Errorf("要回 ErrUserRequired,得到 %v", err)
	}
	if len(repo.bindCalls) != 0 {
		t.Error("不該寫入")
	}
}
