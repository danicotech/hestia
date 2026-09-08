-- 入口層操作紀錄(schemas/13-event-logs.md)。所有進到系統的動作——API / gRPC /
-- Discord bot / webhook / 排程——都在這張表留痕,由各入口的攔截器統一寫,
-- **不在業務碼裡手刻**。內容不存這裡(訊息 = message_logs、錢 = token_entries、
-- 管理 = admin_audit_logs);這張只記「發生過什麼操作、誰、結果、多慢」。
--
-- request / response 是白名單摘要:只放路由層級資訊與業務 public_id,
-- 絕不放 token、密碼或完整 payload。

-- name: InsertEventLog :exec
-- 只有 INSERT:操作紀錄寫下去就不改(同帳本的 append-only 精神),
-- 到期整個分區 DROP,不做逐列刪除。
INSERT INTO platform.event_logs (
  request_id, channel, kind, action, user_id, space_id,
  status, error_code, latency_ms, request, response, trace_id
) VALUES (
  sqlc.narg(request_id), sqlc.arg(channel), sqlc.arg(kind), sqlc.arg(action),
  sqlc.narg(user_id), sqlc.narg(space_id),
  sqlc.narg(status), sqlc.narg(error_code), sqlc.narg(latency_ms),
  sqlc.narg(request), sqlc.narg(response), sqlc.narg(trace_id)
);
