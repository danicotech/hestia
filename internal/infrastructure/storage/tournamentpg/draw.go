package tournamentpg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/danicotech/hestia/internal/core/activity/tournament"
	"github.com/danicotech/hestia/internal/infrastructure/storage/db"
	"github.com/danicotech/hestia/internal/shared/ulid"
)

// 抽籤、退回階段、交換籤位 —— 三個會同時動 tournament_players 與 matches 的操作。
//
// 它們動兩張表的順序天生相反(重抽是先刪 matches 再寫 seed_no,交換是先寫
// seed_no 再改 matches),所以三者一律以 LockTournamentExclusive 開場,
// 對同一屆賽事完全串行化。一屆賽事這三件事只會發生幾次,用一列鎖換掉
// 所有交錯情境很划算(query 檔頭的鎖序第 1 條)。

// RollbackToRanked 執行 drawing → ranked 的回頭路:退階段、刪對戰表、清籤位。
//
// 順序是刻意的:**先擋階段再刪資料**。UpdateTournamentPhase 影響 0 列時
// 什麼都還沒被刪掉,於是「兩個裁判同時按退回」只會有一個真的動到東西,
// 另一個收到 ErrPhaseConflict 而賽事完好如初。
func (s *Service) RollbackToRanked(ctx context.Context, p tournament.RollbackParams) error {
	if p.ActorUserID == 0 {
		return fmt.Errorf("退回段位公布階段: %w", tournament.ErrActorRequired)
	}
	return s.inTx(ctx, func(qtx *db.Queries) error {
		if _, err := s.lockExclusive(ctx, qtx, p.TournamentID); err != nil {
			return err
		}

		n, err := qtx.UpdateTournamentPhase(ctx, db.UpdateTournamentPhaseParams{
			TournamentID: p.TournamentID,
			FromPhase:    string(tournament.PhaseDrawing),
			ToPhase:      string(tournament.PhaseRanked),
		})
		if err != nil {
			return fmt.Errorf("退回賽事 %d 階段: %w", p.TournamentID, err)
		}
		if n == 0 {
			return fmt.Errorf("賽事 %d 不在 %s 階段: %w",
				p.TournamentID, tournament.PhaseDrawing, tournament.ErrPhaseConflict)
		}

		// 對戰表與籤位一起作廢。只做一半的話,觀眾會看到一張已經不作數的表,
		// 而下一次抽籤會撞 UNIQUE (tournament_id, round, slot)。
		if err := s.clearDraw(ctx, qtx, p.TournamentID); err != nil {
			return err
		}

		return writeAudit(ctx, qtx, auditEntry{
			ActorUserID: p.ActorUserID,
			Action:      actionRollback,
			TargetType:  targetTournament,
			TargetID:    p.TournamentID,
			Before:      map[string]any{"phase": string(tournament.PhaseDrawing)},
			After:       map[string]any{"phase": string(tournament.PhaseRanked)},
			Reason:      p.Reason,
		})
	})
}

// ReplaceDraw 寫入一次抽籤結果(重抽也走這裡)。
//
// 種子必須落進稽核紀錄,那是整個抽籤流程唯一的可驗證性來源:
// 裁判被質疑「這籤是不是喬過的」時,拿種子重跑一次就會得到位元相同的表。
// 所以空種子在這裡直接被拒 —— 寫得進去但驗不回來的抽籤,比抽不成更糟。
func (s *Service) ReplaceDraw(ctx context.Context, p tournament.ReplaceDrawParams) error {
	if p.ActorUserID == 0 {
		return fmt.Errorf("抽籤: %w", tournament.ErrActorRequired)
	}
	if p.Seed == "" {
		return fmt.Errorf("抽籤沒有種子就無法重現: %w", tournament.ErrInvalidSeed)
	}
	if len(p.Seats) == 0 {
		return tournament.ErrNoDrawablePlayers
	}

	return s.inTx(ctx, func(qtx *db.Queries) error {
		lock, err := s.lockExclusive(ctx, qtx, p.TournamentID)
		if err != nil {
			return err
		}
		// 取鎖後重讀階段真值:service 那次讀取在取鎖之前,中間裁判可能已經
		// 把賽事推進 in_progress(= 籤表已確認)。那之後再蓋一張新表,
		// 會讓已經發出去的 match_budgets 對到不存在的場次。
		phase, err := tournament.ParsePhase(lock.Phase)
		if err != nil {
			return fmt.Errorf("賽事 %d 的階段 %q: %w", p.TournamentID, lock.Phase, err)
		}
		if err := tournament.RequirePhase(phase, tournament.PhaseDrawing); err != nil {
			return err
		}

		// 重抽要先清乾淨:UNIQUE (tournament_id, seed_no) 是立即檢查的,
		// 不清就重寫會在中途撞上,而且撞在第幾列取決於寫入順序,難以重現。
		if err := s.clearDraw(ctx, qtx, p.TournamentID); err != nil {
			return err
		}

		playerIDs := make([]int64, len(p.Seats))
		seedNos := make([]int32, len(p.Seats))
		for i, seat := range p.Seats {
			playerIDs[i] = seat.PlayerID
			seedNos[i] = seat.SeedNo
		}
		n, err := qtx.AssignSeeds(ctx, db.AssignSeedsParams{
			TournamentID: p.TournamentID,
			PlayerIds:    playerIDs,
			SeedNos:      seedNos,
		})
		if err != nil {
			return fmt.Errorf("寫入賽事 %d 的籤位: %w", p.TournamentID, err)
		}
		if n != int64(len(p.Seats)) {
			// 對不上代表有 player_id 不屬於這屆(或已被刪)。
			// 整筆退回比寫進去一半安全 —— 少一個人的對戰表沒有任何約束會擋。
			return fmt.Errorf("賽事 %d 要寫 %d 個籤位但只影響 %d 列: %w",
				p.TournamentID, len(p.Seats), n, tournament.ErrPlayerNotFound)
		}

		if err := s.insertMatches(ctx, qtx, p.TournamentID, p.Matches); err != nil {
			return err
		}

		return writeAudit(ctx, qtx, auditEntry{
			ActorUserID: p.ActorUserID,
			Action:      actionDraw,
			TargetType:  targetTournament,
			TargetID:    p.TournamentID,
			After: map[string]any{
				// seed 是這筆紀錄存在的理由,不要拿掉。
				"seed":           p.Seed,
				"seat_count":     len(p.Seats),
				"match_count":    len(p.Matches),
				"bye_player_ids": p.ByePlayerIDs,
			},
			Reason: p.Reason,
		})
	})
}

// SwapSeeds 交換兩位選手的籤位。
//
// 兩件事必須同時發生:籤號互換,以及 matches 裡對這兩人的引用互換。
// 只改籤號的話兩張表就互相矛盾,而矛盾的那一刻**沒有任何約束會報錯** ——
// 只是對戰表上的名字跟籤位表對不起來,等到有人發現通常已經開打了。
func (s *Service) SwapSeeds(ctx context.Context, p tournament.SwapSeedsParams) error {
	if p.ActorUserID == 0 {
		return fmt.Errorf("交換籤位: %w", tournament.ErrActorRequired)
	}
	if p.PlayerAID == p.PlayerBID {
		return tournament.ErrSamePlayer
	}

	return s.inTx(ctx, func(qtx *db.Queries) error {
		if _, err := s.lockExclusive(ctx, qtx, p.TournamentID); err != nil {
			return err
		}

		// 依 id 由小到大取列鎖(與全系統同向)。賽事列的獨佔鎖其實已經讓
		// 兩次交換不可能交錯,這裡照規矩來只是為了不留下一個「這裡可以例外」的先例。
		first, second := p.PlayerAID, p.PlayerBID
		if first > second {
			first, second = second, first
		}
		seedFirst, err := s.seedForUpdate(ctx, qtx, p.TournamentID, first)
		if err != nil {
			return err
		}
		seedSecond, err := s.seedForUpdate(ctx, qtx, p.TournamentID, second)
		if err != nil {
			return err
		}

		// 三步而不是一句 CASE 對調:UNIQUE (tournament_id, seed_no) 不是
		// DEFERRABLE,Postgres 在 UPDATE 期間逐列檢查,單一語句直接對調
		// 必然在中途撞鍵。中間那個「first 沒有籤位」的狀態只存在於本 tx 內。
		if err := s.setSeed(ctx, qtx, p.TournamentID, first, nil); err != nil {
			return err
		}
		if err := s.setSeed(ctx, qtx, p.TournamentID, second, &seedFirst); err != nil {
			return err
		}
		if err := s.setSeed(ctx, qtx, p.TournamentID, first, &seedSecond); err != nil {
			return err
		}

		// 對戰表的另一半。沒有對到任何場次是合法的(兩人都還沒進表),
		// 所以這裡不驗影響列數 —— 驗了反而會把正常情況當成錯誤。
		if _, err := qtx.SwapPlayersInMatches(ctx, db.SwapPlayersInMatchesParams{
			PlayerA:      p.PlayerAID,
			PlayerB:      p.PlayerBID,
			TournamentID: p.TournamentID,
		}); err != nil {
			return fmt.Errorf("交換賽事 %d 對戰表上的選手: %w", p.TournamentID, err)
		}

		return writeAudit(ctx, qtx, auditEntry{
			ActorUserID: p.ActorUserID,
			Action:      actionSwapSeeds,
			TargetType:  targetTournament,
			TargetID:    p.TournamentID,
			Before: map[string]any{
				"player_a": first, "seed_a": seedFirst,
				"player_b": second, "seed_b": seedSecond,
			},
			After: map[string]any{
				"player_a": first, "seed_a": seedSecond,
				"player_b": second, "seed_b": seedFirst,
			},
			Reason: p.Reason,
		})
	})
}

// ── 共用步驟 ──────────────────────────────────────────────────

// lockExclusive 是破壞性操作的第一步:鎖住賽事列並回傳當下的階段。
func (s *Service) lockExclusive(ctx context.Context, qtx *db.Queries, tournamentID int64) (db.LockTournamentExclusiveRow, error) {
	row, err := qtx.LockTournamentExclusive(ctx, tournamentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return row, fmt.Errorf("賽事 %d: %w", tournamentID, tournament.ErrTournamentNotFound)
		}
		return row, fmt.Errorf("鎖賽事 %d: %w", tournamentID, err)
	}
	return row, nil
}

// clearDraw 清掉一屆賽事既有的對戰表與籤位。
//
// 順序固定 matches → seed_no:matches 對 tournament_players 有 FK,
// 反過來清會踩到「籤位已經沒了但場次還指著那些人」的中間狀態。
// handicap / bets 對 matches 也有 FK,所以「已經有人下過注的對戰表」
// 會在刪除時被資料庫擋下來 —— 那正是要的:drawing 階段本來就不該有注單。
func (s *Service) clearDraw(ctx context.Context, qtx *db.Queries, tournamentID int64) error {
	if err := qtx.DeleteTournamentMatches(ctx, tournamentID); err != nil {
		return fmt.Errorf("刪除賽事 %d 的對戰表: %w", tournamentID, err)
	}
	if err := qtx.ClearTournamentSeeds(ctx, tournamentID); err != nil {
		return fmt.Errorf("清空賽事 %d 的籤位: %w", tournamentID, err)
	}
	return nil
}

// insertMatches 整棵樹一個語句寫完,public_id 由這裡逐場產生。
func (s *Service) insertMatches(ctx context.Context, qtx *db.Queries, tournamentID int64, seats []tournament.MatchSeat) error {
	if len(seats) == 0 {
		// 只有一人進表時是合法的退化情況:零場比賽,那個人直接是冠軍。
		return nil
	}
	// 五個平行陣列**必須在同一個迴圈裡組出來**:長度不一致時 Postgres 會
	// 補 NULL 而不是報錯,分開組的話某天少 append 一次就是一張悄悄錯掉的表。
	publicIDs := make([]string, 0, len(seats))
	rounds := make([]int32, 0, len(seats))
	slots := make([]int32, 0, len(seats))
	p1s := make([]int64, 0, len(seats))
	p2s := make([]int64, 0, len(seats))
	for _, m := range seats {
		pubID, err := ulid.New()
		if err != nil {
			return fmt.Errorf("產生場次 public_id: %w", err)
		}
		publicIDs = append(publicIDs, pubID)
		rounds = append(rounds, m.Round)
		slots = append(slots, m.Slot)
		p1s = append(p1s, m.P1PlayerID)
		p2s = append(p2s, m.P2PlayerID)
	}

	n, err := qtx.InsertMatches(ctx, db.InsertMatchesParams{
		TournamentID: tournamentID,
		PublicIds:    publicIDs,
		Rounds:       rounds,
		Slots:        slots,
		P1PlayerIds:  p1s,
		P2PlayerIds:  p2s,
	})
	if err != nil {
		return fmt.Errorf("寫入賽事 %d 的對戰表: %w", tournamentID, err)
	}
	if n != int64(len(seats)) {
		return fmt.Errorf("賽事 %d 要寫 %d 場但只影響 %d 列", tournamentID, len(seats), n)
	}
	return nil
}

// seedForUpdate 在 tx 內重讀一位選手的籤位並鎖住該列。
func (s *Service) seedForUpdate(ctx context.Context, qtx *db.Queries, tournamentID, playerID int64) (int32, error) {
	row, err := qtx.GetPlayerSeedForUpdate(ctx, db.GetPlayerSeedForUpdateParams{
		PlayerID: playerID, TournamentID: tournamentID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("賽事 %d 的選手 %d: %w",
				tournamentID, playerID, tournament.ErrPlayerNotFound)
		}
		return 0, fmt.Errorf("讀選手 %d 的籤位: %w", playerID, err)
	}
	if row.SeedNo == nil {
		// 沒有籤位的人不能「交換」—— 那會變成把對方的籤位讓出去,
		// 留下一張少一個人的對戰表,而且沒有任何約束會擋。
		return 0, fmt.Errorf("選手 %d: %w", playerID, tournament.ErrPlayerNotSeeded)
	}
	return *row.SeedNo, nil
}

// setSeed 寫一位選手的籤位(nil = 清空)。影響列數必為 1:
// 上一步才剛用 FOR UPDATE 讀到並鎖住這一列。
func (s *Service) setSeed(ctx context.Context, qtx *db.Queries, tournamentID, playerID int64, seed *int32) error {
	n, err := qtx.SetPlayerSeed(ctx, db.SetPlayerSeedParams{
		SeedNo: seed, PlayerID: playerID, TournamentID: tournamentID,
	})
	if err != nil {
		return fmt.Errorf("寫入選手 %d 的籤位: %w", playerID, err)
	}
	if n != 1 {
		return fmt.Errorf("寫入選手 %d 的籤位影響 %d 列,預期 1 列", playerID, n)
	}
	return nil
}
