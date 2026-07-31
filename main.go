package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"wx-code-getter/internal/protocol"
	"wx-code-getter/internal/qr"
)

var (
	qrClient   *qr.Client
	codeGetter *protocol.CodeGetter

	// 扫码会话缓存
	qrSessions   = map[string]*qr.Session{}
	qrSessionsMu sync.Mutex

	// 已确认的登录会话（login_buffer）
	loginSessions   = map[string]*loginSession{} // key = session_id
	loginSessionsMu sync.Mutex
)

type loginSession struct {
	LoginBuffer string
	OpenID      string
	Nickname    string
	CreatedAt   time.Time
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	qrClient = qr.NewClient(8 * time.Second)

	cfg := protocol.DefaultConfig()
	cfg.SessionTTL = 30 * time.Minute
	codeGetter = protocol.NewCodeGetter(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/qr", handleQR)
	mux.HandleFunc("/api/qr/poll", handleQRPoll)
	mux.HandleFunc("/api/qr/confirm", handleQRConfirm)
	mux.HandleFunc("/api/getCode", handleGetCode)
	mux.HandleFunc("/health", handleHealth)

	addr := ":" + port
	log.Printf("Server starting on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(indexHTML))
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func handleQR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	img, err := qrClient.GetQRCodeImage(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	qrSessionsMu.Lock()
	qrSessions[img.Session.ID] = img.Session
	qrSessionsMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": img.Session.ID,
		"status":     img.Session.Status,
		"image_base64": qr.DataURIJPEG(img.ImageBytes),
	})
}

func handleQRPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}

	qrSessionsMu.Lock()
	sess, ok := qrSessions[sessionID]
	qrSessionsMu.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	result, err := qrClient.PollQRCode(r.Context(), sess)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func handleQRConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if req.SessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}

	qrSessionsMu.Lock()
	sess, ok := qrSessions[req.SessionID]
	qrSessionsMu.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	result, err := qrClient.GetLoginBuffer(r.Context(), sess)
	if err != nil {
		writeError(w, http.StatusConflict, "buffer not ready: "+err.Error())
		return
	}

	// 存储登录会话
	ls := &loginSession{
		LoginBuffer: result.LoginBuffer,
		OpenID:      result.Credentials.OpenID,
		Nickname:    result.Credentials.Nickname,
		CreatedAt:   time.Now(),
	}
	loginSessionsMu.Lock()
	loginSessions[req.SessionID] = ls
	loginSessionsMu.Unlock()

	// 清理 QR 会话
	qrSessionsMu.Lock()
	delete(qrSessions, req.SessionID)
	qrSessionsMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": req.SessionID,
		"openid":     result.Credentials.OpenID,
		"nickname":   result.Credentials.Nickname,
	})
}

func handleGetCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		AppID     string `json:"app_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if req.SessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	if req.AppID == "" {
		writeError(w, http.StatusBadRequest, "app_id is required")
		return
	}

	loginSessionsMu.Lock()
	ls, ok := loginSessions[req.SessionID]
	loginSessionsMu.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "session not found, please scan QR code first")
		return
	}

	result, err := codeGetter.GetCode(r.Context(), ls.LoginBuffer, req.AppID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "get code failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": msg,
	})
}

const indexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>微信扫码获取 Code</title>
  <style>
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "Microsoft YaHei", sans-serif;
      background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 20px;
    }
    .container {
      background: #fff;
      border-radius: 16px;
      box-shadow: 0 20px 60px rgba(0,0,0,0.3);
      width: 100%;
      max-width: 440px;
      overflow: hidden;
    }
    .header {
      background: linear-gradient(135deg, #07c160 0%, #06ad56 100%);
      color: #fff;
      padding: 24px;
      text-align: center;
    }
    .header h1 {
      font-size: 20px;
      font-weight: 600;
      margin-bottom: 4px;
    }
    .header p {
      font-size: 13px;
      opacity: 0.9;
    }
    .body {
      padding: 28px 24px;
    }
    .step {
      display: none;
    }
    .step.active {
      display: block;
    }
    .qr-box {
      width: 240px;
      height: 240px;
      margin: 0 auto 20px;
      border: 2px solid #e8f5e9;
      border-radius: 12px;
      display: flex;
      align-items: center;
      justify-content: center;
      background: #fafafa;
      overflow: hidden;
    }
    .qr-box img {
      width: 100%;
      height: 100%;
      object-fit: contain;
    }
    .qr-loading {
      color: #999;
      font-size: 14px;
    }
    .status {
      text-align: center;
      font-size: 14px;
      color: #666;
      margin-bottom: 20px;
      min-height: 20px;
    }
    .status.scanned { color: #07c160; }
    .status.error { color: #f56c6c; }
    .btn {
      width: 100%;
      padding: 12px;
      border: none;
      border-radius: 8px;
      font-size: 15px;
      font-weight: 500;
      cursor: pointer;
      transition: all 0.2s;
    }
    .btn-primary {
      background: #07c160;
      color: #fff;
    }
    .btn-primary:hover { background: #06ad56; }
    .btn-primary:disabled {
      background: #a8d8b9;
      cursor: not-allowed;
    }
    .btn-secondary {
      background: #f5f5f5;
      color: #666;
      margin-top: 10px;
    }
    .btn-secondary:hover { background: #eee; }
    .user-info {
      text-align: center;
      margin-bottom: 24px;
      padding: 16px;
      background: #f0f9eb;
      border-radius: 10px;
    }
    .user-info .nickname {
      font-size: 16px;
      font-weight: 600;
      color: #07c160;
      margin-bottom: 4px;
    }
    .user-info .openid {
      font-size: 12px;
      color: #999;
      font-family: monospace;
    }
    .form-group {
      margin-bottom: 20px;
    }
    .form-group label {
      display: block;
      font-size: 13px;
      color: #666;
      margin-bottom: 8px;
      font-weight: 500;
    }
    .form-group input {
      width: 100%;
      padding: 11px 14px;
      border: 1px solid #ddd;
      border-radius: 8px;
      font-size: 14px;
      outline: none;
      transition: border-color 0.2s;
    }
    .form-group input:focus {
      border-color: #07c160;
    }
    .code-result {
      background: #f8f9fa;
      border: 1px solid #e9ecef;
      border-radius: 10px;
      padding: 16px;
      margin-bottom: 16px;
    }
    .code-result .label {
      font-size: 12px;
      color: #999;
      margin-bottom: 8px;
    }
    .code-result .code-text {
      font-family: "SF Mono", Monaco, "Courier New", monospace;
      font-size: 13px;
      color: #333;
      word-break: break-all;
      line-height: 1.6;
      background: #fff;
      padding: 10px 12px;
      border-radius: 6px;
      border: 1px solid #eee;
      user-select: all;
    }
    .copy-btn {
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 6px;
      margin-top: 10px;
      padding: 8px 16px;
      background: #e8f5e9;
      color: #07c160;
      border: none;
      border-radius: 6px;
      font-size: 13px;
      cursor: pointer;
      transition: all 0.2s;
      width: 100%;
    }
    .copy-btn:hover { background: #c8e6c9; }
    .copy-btn.copied { background: #07c160; color: #fff; }
    .loading-dots::after {
      content: '';
      animation: dots 1.5s steps(4, end) infinite;
    }
    @keyframes dots {
      0%, 20% { content: ''; }
      40% { content: '.'; }
      60% { content: '..'; }
      80%, 100% { content: '...'; }
    }
    .hint {
      font-size: 12px;
      color: #999;
      text-align: center;
      margin-top: 12px;
      line-height: 1.5;
    }
  </style>
</head>
<body>
  <div class="container">
    <div class="header">
      <h1>微信扫码获取 Code</h1>
      <p>扫码登录 → 输入 AppID → 获取 Code</p>
    </div>
    <div class="body">
      <!-- 步骤1：扫码 -->
      <div id="step1" class="step active">
        <div class="qr-box" id="qrBox">
          <span class="qr-loading">生成中<span class="loading-dots"></span></span>
        </div>
        <div class="status" id="status">正在生成二维码...</div>
        <button class="btn btn-secondary" id="refreshBtn" style="display:none;">重新生成二维码</button>
        <p class="hint">使用微信扫码并确认登录<br>Code 仅在本地显示，不会上传到其他地方</p>
      </div>

      <!-- 步骤2：输入 AppID 获取 Code -->
      <div id="step2" class="step">
        <div class="user-info">
          <div class="nickname" id="nickname">用户昵称</div>
          <div class="openid" id="openid">openid</div>
        </div>
        <div class="form-group">
          <label>小程序 AppID</label>
          <input type="text" id="appIdInput" placeholder="请输入小程序 AppID，如 wx1234567890abcdef">
        </div>
        <button class="btn btn-primary" id="getCodeBtn">获取 Code</button>
        <button class="btn btn-secondary" id="backBtn">重新扫码</button>
      </div>

      <!-- 步骤3：显示 Code -->
      <div id="step3" class="step">
        <div class="user-info">
          <div class="nickname" id="nickname2">用户昵称</div>
          <div class="openid" id="openid2">openid</div>
        </div>
        <div class="code-result">
          <div class="label">小程序 Code</div>
          <div class="code-text" id="codeText">--</div>
          <button class="copy-btn" id="copyBtn">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
              <rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect>
              <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path>
            </svg>
            复制 Code
          </button>
        </div>
        <div class="form-group">
          <label>换个 AppID？</label>
          <input type="text" id="appIdInput2" placeholder="输入新的 AppID">
        </div>
        <button class="btn btn-primary" id="getCodeBtn2">重新获取 Code</button>
        <button class="btn btn-secondary" id="backBtn2">重新扫码</button>
      </div>
    </div>
  </div>

  <script>
    let sessionId = null;
    let pollTimer = null;

    const $ = id => document.getElementById(id);

    function showStep(n) {
      document.querySelectorAll('.step').forEach(s => s.classList.remove('active'));
      $('step' + n).classList.add('active');
    }

    async function api(url, options = {}) {
      const r = await fetch(url, {
        headers: { 'Content-Type': 'application/json' },
        ...options
      });
      const data = await r.json();
      if (!r.ok) {
        throw new Error(data.error || ('HTTP ' + r.status));
      }
      return data;
    }

    async function newQR() {
      $('qrBox').innerHTML = '<span class="qr-loading">生成中<span class="loading-dots"></span></span>';
      $('status').textContent = '正在生成二维码...';
      $('status').className = 'status';
      $('refreshBtn').style.display = 'none';

      try {
        const r = await api('/api/qr', { method: 'POST' });
        sessionId = r.session_id;
        $('qrBox').innerHTML = '<img src="' + r.image_base64 + '" alt="二维码">';
        $('status').textContent = '请使用微信扫码';
        startPoll();
      } catch (e) {
        $('status').textContent = '生成失败: ' + e.message;
        $('status').className = 'status error';
        $('refreshBtn').style.display = 'block';
      }
    }

    function startPoll() {
      if (pollTimer) clearInterval(pollTimer);
      pollTimer = setInterval(poll, 1500);
    }

    function stopPoll() {
      if (pollTimer) {
        clearInterval(pollTimer);
        pollTimer = null;
      }
    }

    async function poll() {
      if (!sessionId) return;
      try {
        const st = await api('/api/qr/poll?session_id=' + sessionId);
        if (st.status === 'pending') {
          $('status').textContent = '请使用微信扫码';
        } else if (st.status === 'scanned') {
          $('status').textContent = '已扫码，请在手机上确认';
          $('status').className = 'status scanned';
        } else if (st.status === 'authorized' || st.status === 'confirmed') {
          stopPoll();
          await confirmLogin();
        } else if (st.status === 'expired') {
          stopPoll();
          $('status').textContent = '二维码已过期';
          $('status').className = 'status error';
          $('refreshBtn').style.display = 'block';
        } else if (st.status === 'cancelled') {
          stopPoll();
          $('status').textContent = '已取消，请重新扫码';
          $('status').className = 'status error';
          $('refreshBtn').style.display = 'block';
        }
      } catch (e) {
        // 忽略轮询错误
      }
    }

    async function confirmLogin() {
      $('status').textContent = '登录中...';
      try {
        const r = await api('/api/qr/confirm', {
          method: 'POST',
          body: JSON.stringify({ session_id: sessionId })
        });
        $('nickname').textContent = r.nickname || '微信用户';
        $('openid').textContent = r.openid || '';
        $('nickname2').textContent = r.nickname || '微信用户';
        $('openid2').textContent = r.openid || '';
        showStep(2);
      } catch (e) {
        $('status').textContent = '登录失败: ' + e.message;
        $('status').className = 'status error';
        $('refreshBtn').style.display = 'block';
      }
    }

    async function getCode(appId) {
      const r = await api('/api/getCode', {
        method: 'POST',
        body: JSON.stringify({ session_id: sessionId, app_id: appId })
      });
      return r.code;
    }

    $('refreshBtn').addEventListener('click', newQR);

    $('getCodeBtn').addEventListener('click', async () => {
      const appId = $('appIdInput').value.trim();
      if (!appId) {
        alert('请输入 AppID');
        return;
      }
      $('getCodeBtn').disabled = true;
      $('getCodeBtn').textContent = '获取中...';
      try {
        const code = await getCode(appId);
        $('codeText').textContent = code;
        $('appIdInput2').value = appId;
        showStep(3);
      } catch (e) {
        alert('获取失败: ' + e.message);
      } finally {
        $('getCodeBtn').disabled = false;
        $('getCodeBtn').textContent = '获取 Code';
      }
    });

    $('getCodeBtn2').addEventListener('click', async () => {
      const appId = $('appIdInput2').value.trim();
      if (!appId) {
        alert('请输入 AppID');
        return;
      }
      $('getCodeBtn2').disabled = true;
      $('getCodeBtn2').textContent = '获取中...';
      try {
        const code = await getCode(appId);
        $('codeText').textContent = code;
      } catch (e) {
        alert('获取失败: ' + e.message);
      } finally {
        $('getCodeBtn2').disabled = false;
        $('getCodeBtn2').textContent = '重新获取 Code';
      }
    });

    $('copyBtn').addEventListener('click', async () => {
      const code = $('codeText').textContent;
      try {
        await navigator.clipboard.writeText(code);
        $('copyBtn').classList.add('copied');
        $('copyBtn').innerHTML = '✓ 已复制';
        setTimeout(() => {
          $('copyBtn').classList.remove('copied');
          $('copyBtn').innerHTML = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg> 复制 Code';
        }, 2000);
      } catch (e) {
        alert('复制失败，请手动复制');
      }
    });

    function reset() {
      stopPoll();
      sessionId = null;
      $('appIdInput').value = '';
      $('appIdInput2').value = '';
      $('codeText').textContent = '--';
      showStep(1);
      newQR();
    }

    $('backBtn').addEventListener('click', reset);
    $('backBtn2').addEventListener('click', reset);

    // 启动
    newQR();
  </script>
</body>
</html>`
