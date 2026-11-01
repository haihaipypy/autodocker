package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// ActionHandler 处理一次卡片按钮点击。
//
// 注意：飞书要求回调在 3 秒内返回，实现方必须在内部异步执行耗时操作，
// 只把「已受理」的状态卡片返回去。
type ActionHandler interface {
	HandleCardAction(ctx context.Context, v CardValue) (toast string, card map[string]any)
}

// LongConn 基于 WebSocket 长连接的卡片回调接收器。
//
// 采用长连接的好处：不需要公网地址、不需要配置回调 URL，容器部署即用。
// 限制：仅企业自建应用可用；同一应用多实例部署时事件随机投递到其中一个连接。
type LongConn struct {
	appID     string
	appSecret string
	handler   ActionHandler
	log       *slog.Logger
	client    *larkws.Client
}

// NewLongConn 构造长连接接收器。
func NewLongConn(appID, appSecret string, h ActionHandler, logger *slog.Logger) *LongConn {
	return &LongConn{appID: appID, appSecret: appSecret, handler: h, log: logger}
}

// Start 建立长连接并阻塞，直到 ctx 结束。
func (l *LongConn) Start(ctx context.Context) error {
	if l.appID == "" || l.appSecret == "" {
		l.log.Warn("飞书应用未配置，卡片交互长连接不会启动")
		return nil
	}

	evDispatcher := dispatcher.NewEventDispatcher("", "").
		OnP2CardActionTrigger(l.onCardAction)

	l.client = larkws.NewClient(
		l.appID,
		l.appSecret,
		larkws.WithEventHandler(evDispatcher),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
	)

	l.log.Info("飞书卡片交互长连接启动中")
	err := l.client.Start(ctx)
	if err != nil {
		// 关停时 ctx 被取消，SDK 把 context canceled 当错误返回来。
		// 那不是故障 —— 打成 ERROR 等于每次重启都留一条假的「异常」，
		// 用户按文档来查日志时会被它带偏，真出问题时反而分不清。
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			l.log.Info("飞书长连接已关闭")
		} else {
			l.log.Error("飞书长连接异常退出", "err", err)
		}
	}
	return err
}

// LongConnManager 管理长连接的生命周期。
//
// 存在的理由：应用凭据是装好之后在网页里填的，服务启动那一刻往往还没有凭据。
// 若只在启动时拉一次长连接，用户填完凭据还得重启应用才能收到按钮回调；
// 反过来，改错凭据也没有办法在不重启的情况下修正。
// 这里把「凭据 → 长连接」做成可随时收敛的状态，保存设置后即时生效。
type LongConnManager struct {
	baseCtx context.Context
	handler ActionHandler
	log     *slog.Logger

	mu        sync.Mutex
	appID     string
	appSecret string
	cancel    context.CancelFunc
	running   bool
	gen       int // 累计实际发起连接的次数，仅用于观测与测试
}

// NewLongConnManager 构造长连接管理器。
func NewLongConnManager(baseCtx context.Context, h ActionHandler, logger *slog.Logger) *LongConnManager {
	return &LongConnManager{baseCtx: baseCtx, handler: h, log: logger}
}

// Ensure 让长连接收敛到给定凭据对应的状态。
//
// 凭据没变时直接返回，所以反复保存其它设置不会无谓地重连
// （重连会短暂丢失按钮回调，频繁重连还可能触发飞书的连接数限制）。
func (m *LongConnManager) Ensure(appID, appSecret string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if appID == m.appID && appSecret == m.appSecret {
		return
	}

	// 凭据变了：先停掉旧的，再决定要不要起新的
	m.stopLocked()
	m.appID, m.appSecret = appID, appSecret

	if appID == "" || appSecret == "" {
		m.log.Info("飞书凭据不完整，卡片交互长连接保持关闭")
		return
	}

	m.gen++ // 仅用于观测「实际发起过几次连接」

	ctx, cancel := context.WithCancel(m.baseCtx)
	m.cancel = cancel
	m.running = true

	lc := NewLongConn(appID, appSecret, m.handler, m.log)
	go func() {
		defer func() {
			// 主动停止（ctx 已取消）时不动 running：那多半是紧接着要换新连接，
			// 这里再置一次 false 会把后来者的运行状态误标成已停止。
			if ctx.Err() == nil {
				m.mu.Lock()
				m.running = false
				m.mu.Unlock()
			}
		}()
		if err := lc.Start(ctx); err != nil && ctx.Err() == nil {
			m.log.Error("飞书卡片长连接退出", "err", err)
		}
	}()
}

// Stop 关闭长连接（用于优雅退出）。
func (m *LongConnManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

// stopLocked 停掉当前连接。调用方必须已持有锁。
func (m *LongConnManager) stopLocked() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.running = false
}

// Running 返回长连接当前是否已拉起。
func (m *LongConnManager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// onCardAction 处理卡片回传交互事件。
func (l *LongConn) onCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return nil, nil
	}

	value, err := toCardValue(event.Event.Action.Value)
	if err != nil {
		l.log.Warn("解析卡片回传数据失败", "err", err)
		return &callback.CardActionTriggerResponse{
			Toast: &callback.Toast{Type: "error", Content: "操作数据无效"},
		}, nil
	}

	// 事件上下文在响应返回后即失效，这里用独立的 Background 交给业务侧异步处理
	toast, card := l.handler.HandleCardAction(context.Background(), value)

	resp := &callback.CardActionTriggerResponse{}
	if toast != "" {
		resp.Toast = &callback.Toast{Type: "success", Content: toast}
	}
	if card != nil {
		resp.Card = &callback.Card{Type: "raw", Data: card}
	}
	return resp, nil
}

// toCardValue 把回调里的松散 map 转成结构化载荷。
// 走一次 JSON 往返，避免手动处理 float64 与 int64 的差异。
func toCardValue(m map[string]interface{}) (CardValue, error) {
	var v CardValue
	if len(m) == 0 {
		return v, nil
	}
	buf, err := json.Marshal(m)
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(buf, &v); err != nil {
		return v, err
	}
	return v, nil
}
