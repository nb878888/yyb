package protocol

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Config 简化配置，去掉数据库相关
type Config struct {
	ShortlinkTimeout        time.Duration
	LoginTimeout            time.Duration
	SessionTTL              time.Duration
	DNSCacheTTL             time.Duration
	MaxShortlinkConcurrency int
	MaxLoginConcurrency     int
	TCPProxy                string
	TCPProxyFallbackDirect  bool
}

func DefaultConfig() Config {
	return Config{
		ShortlinkTimeout:        8 * time.Second,
		LoginTimeout:            30 * time.Second,
		SessionTTL:              30 * time.Minute,
		DNSCacheTTL:             30 * time.Minute,
		MaxShortlinkConcurrency: 1000,
		MaxLoginConcurrency:     32,
		TCPProxyFallbackDirect:  true,
	}
}

// WmpfSession 会话数据
type WmpfSession struct {
	Session          AppSession
	PSK              pskEntry
	ShortlinkTargets []Target
	CreatedAt        time.Time
	TCPProxy         string
}

// CodeGetter 简化版，只保留获取 code 功能，无数据库依赖
type CodeGetter struct {
	cfg Config

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// 内存 session 缓存，key = login_buffer
	sessions map[string]*WmpfSession

	loginSem     chan struct{}
	shortlinkSem chan struct{}
}

func NewCodeGetter(cfg Config) *CodeGetter {
	def := DefaultConfig()
	if cfg.ShortlinkTimeout == 0 {
		cfg.ShortlinkTimeout = def.ShortlinkTimeout
	}
	if cfg.LoginTimeout == 0 {
		cfg.LoginTimeout = def.LoginTimeout
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = def.SessionTTL
	}
	if cfg.DNSCacheTTL == 0 {
		cfg.DNSCacheTTL = def.DNSCacheTTL
	}
	if cfg.MaxShortlinkConcurrency == 0 {
		cfg.MaxShortlinkConcurrency = def.MaxShortlinkConcurrency
	}
	if cfg.MaxLoginConcurrency == 0 {
		cfg.MaxLoginConcurrency = def.MaxLoginConcurrency
	}
	return &CodeGetter{
		cfg:          cfg,
		locks:        map[string]*sync.Mutex{},
		sessions:     map[string]*WmpfSession{},
		loginSem:     make(chan struct{}, cfg.MaxLoginConcurrency),
		shortlinkSem: make(chan struct{}, cfg.MaxShortlinkConcurrency),
	}
}

// GetCode 获取小程序 code
func (g *CodeGetter) GetCode(ctx context.Context, loginBuffer, appID string) (map[string]any, error) {
	return g.run(ctx, loginBuffer, func(ctx context.Context, st WmpfSession) (map[string]any, error) {
		hostAppID := st.Session.HostAppID
		if len(hostAppID) == 0 {
			hostAppID = hostAppIDDefault
		}
		plain := buildJSAPIPlaintext(st.Session.UIN, appID, jsLoginURL, jsLoginCmdID, nil, hostAppID, nil)
		envelope, err := buildTransferPacket(st.Session, plain)
		if err != nil {
			return nil, err
		}
		code, _, err := g.sendEnvelope(ctx, st, envelope)
		if err != nil {
			return nil, err
		}
		return map[string]any{"code": string(code), "errMsg": "login:ok"}, nil
	})
}

func (g *CodeGetter) run(ctx context.Context, loginBuffer string, op func(context.Context, WmpfSession) (map[string]any, error)) (map[string]any, error) {
	effective := g.cfg.TCPProxy
	st, err := g.state(ctx, loginBuffer, effective)
	if err == nil {
		res, err := op(ctx, st)
		if err == nil || effective == "" || !g.cfg.TCPProxyFallbackDirect {
			return res, err
		}
		// fallback: 清除缓存的 session
		g.mu.Lock()
		delete(g.sessions, loginBuffer)
		g.mu.Unlock()
	}
	if effective != "" && g.cfg.TCPProxyFallbackDirect {
		st, err = g.state(ctx, loginBuffer, "")
		if err != nil {
			return nil, err
		}
		return op(ctx, st)
	}
	return nil, err
}

func (g *CodeGetter) state(ctx context.Context, loginBuffer string, tcpProxy string) (WmpfSession, error) {
	// 先查内存缓存
	g.mu.Lock()
	if st, ok := g.sessions[loginBuffer]; ok {
		if time.Since(st.CreatedAt) < g.cfg.SessionTTL {
			g.mu.Unlock()
			return *st, nil
		}
		// 过期了，删除
		delete(g.sessions, loginBuffer)
	}
	g.mu.Unlock()

	// 加锁，避免并发登录同一个账号
	lock := g.lockFor(loginBuffer)
	lock.Lock()
	defer lock.Unlock()

	// 再次检查缓存（双重检查）
	g.mu.Lock()
	if st, ok := g.sessions[loginBuffer]; ok {
		if time.Since(st.CreatedAt) < g.cfg.SessionTTL {
			g.mu.Unlock()
			return *st, nil
		}
	}
	g.mu.Unlock()

	if loginBuffer == "" {
		return WmpfSession{}, fmt.Errorf("login_buffer is empty")
	}

	if err := acquire(ctx, g.loginSem); err != nil {
		return WmpfSession{}, err
	}
	defer release(g.loginSem)

	loginCtx, cancel := context.WithTimeout(ctx, g.cfg.LoginTimeout)
	defer cancel()

	st, err := g.loginAndSession(loginCtx, loginBuffer, tcpProxy)
	if err != nil {
		return WmpfSession{}, err
	}

	// 存入缓存
	g.mu.Lock()
	g.sessions[loginBuffer] = &st
	g.mu.Unlock()

	return st, nil
}

func (g *CodeGetter) lockFor(key string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l := g.locks[key]; l != nil {
		return l
	}
	l := &sync.Mutex{}
	g.locks[key] = l
	return l
}

func (g *CodeGetter) loginAndSession(ctx context.Context, loginBuffer, tcpProxy string) (WmpfSession, error) {
	targets, err := getLonglinkTargets(ctx, g.cfg.LoginTimeout, g.cfg.DNSCacheTTL)
	if err != nil {
		return WmpfSession{}, fmt.Errorf("HTTPDNS LongLink failed: %w", err)
	}
	targets = orderLonglinkTargets(targets, 6)

	var last error
	for _, t := range targets {
		mc, err := connectMmtls(ctx, t, g.cfg.LoginTimeout, tcpProxy, g.cfg.TCPProxyFallbackDirect)
		if err != nil {
			last = err
			continue
		}
		defer mc.close()

		meta, err := parseLoginBuffer(loginBuffer)
		if err != nil {
			return WmpfSession{}, err
		}

		appDeviceID, err := randomAppDeviceID()
		if err != nil {
			return WmpfSession{}, err
		}

		temp := &manualAuthTemp{}
		body, err := buildLoginBody(loginBuffer, meta.DeviceID, appDeviceID, temp)
		if err != nil {
			return WmpfSession{}, err
		}

		if err = mc.sendApp(cmdManualAuth, body); err != nil {
			last = err
			continue
		}

		resp, err := mc.recvApp()
		if err != nil {
			last = err
			continue
		}

		if resp.Cmd != cmdManualAuth {
			last = fmt.Errorf("manualauth failed: cmd=%d", resp.Cmd)
			continue
		}

		mar, err := parseLoginResponse(resp.Body, temp)
		if err != nil {
			last = err
			continue
		}

		appSess, err := extractSession(mar)
		if err != nil {
			last = err
			continue
		}

		appSess.DeviceID = meta.DeviceID
		appSess.HostAppID = meta.HostAppID

		psks, err := mc.extractPSKs()
		if err != nil {
			last = err
			continue
		}

		psk, ok := pickAccessPSK(psks)
		if !ok {
			last = fmt.Errorf("login finished but no access PSK was issued")
			continue
		}

		shortTargets := getShortlinkTargets(ctx, g.cfg.ShortlinkTimeout, g.cfg.DNSCacheTTL)

		return WmpfSession{
			Session:          appSess,
			PSK:              psk,
			ShortlinkTargets: shortTargets,
			CreatedAt:        time.Now(),
			TCPProxy:         tcpProxy,
		}, nil
	}

	if last == nil {
		last = fmt.Errorf("no LongLink candidates")
	}
	return WmpfSession{}, fmt.Errorf("all LongLink candidates failed: %w", last)
}

func (g *CodeGetter) sendEnvelope(ctx context.Context, st WmpfSession, envelope []byte) ([]byte, []byte, error) {
	if err := acquire(ctx, g.shortlinkSem); err != nil {
		return nil, nil, err
	}
	defer release(g.shortlinkSem)

	reqCtx, cancel := context.WithTimeout(ctx, g.cfg.ShortlinkTimeout)
	defer cancel()

	fallback := g.cfg.TCPProxyFallbackDirect
	if st.TCPProxy != "" {
		fallback = false
	}

	return send0RTT(reqCtx, st.ShortlinkTargets, st.PSK, st.Session.RecvKey, envelope, g.cfg.ShortlinkTimeout, st.TCPProxy, fallback)
}

func acquire(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func release(sem chan struct{}) {
	select {
	case <-sem:
	default:
	}
}
