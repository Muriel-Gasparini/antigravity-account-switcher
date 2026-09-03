// Antigravity Account Switcher - Production Dashboard Application
// Server-driven • Offline-first • Tabular-nums • Zero-clipping Tooltip
(function () {
  'use strict';

  // Application State
  let currentPeriod = 'lifetime';
  let eventSource = null;
  let rawTimelineData = [];
  let rawAccountsData = [];
  let allEvents = [];
  let currentLogFilter = 'all';
  let isUserScrolledUp = false;
  let selectedColumnIndex = -1;

  // DOM Elements
  const logsContainer = document.getElementById('logs-container');
  const accountsGrid = document.getElementById('accounts-grid');
  const toastContainer = document.getElementById('toast-container');
  const chartCard = document.getElementById('timeline-chart-card');
  const chartViewport = document.getElementById('chart-viewport');
  const barsTrack = document.getElementById('bars-track');
  const chartInspector = document.getElementById('chart-inspector');
  const inspectorDate = document.getElementById('inspector-date');
  const inspectorData = document.getElementById('inspector-data');
  const chartTooltip = document.getElementById('chart-tooltip');
  const chartXAxis = document.getElementById('chart-x-axis');
  const gridMaxLabel = document.getElementById('grid-max-label');
  const gridMidLabel = document.getElementById('grid-mid-label');

  // =========================================================================
  // Formatting & Utility Helpers
  // =========================================================================

  function escapeHtml(str) {
    if (str === null || str === undefined) return '';
    return String(str)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#039;');
  }

  function formatNumber(num) {
    if (num === null || num === undefined || isNaN(num)) return '0';
    return Number(num).toLocaleString('en-US');
  }

  function formatCompactNumber(num) {
    if (!num || isNaN(num)) return '0';
    const n = Number(num);
    if (n >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, '') + 'B';
    if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'K';
    return n.toLocaleString('en-US');
  }

  function formatTimeOnly(isoStr) {
    if (!isoStr) return '--:--:--';
    const d = new Date(isoStr);
    if (isNaN(d.getTime())) return '--:--:--';
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
  }

  function formatDateLabel(isoDateStr) {
    if (!isoDateStr) return '--';
    const parts = isoDateStr.split('-');
    if (parts.length === 3) {
      const d = new Date(Number(parts[0]), Number(parts[1]) - 1, Number(parts[2]));
      return d.toLocaleDateString([], { month: 'short', day: 'numeric' });
    }
    return isoDateStr;
  }

  // Calculate live countdown timer from reset time string
  function getRemainingSeconds(resetTimeStr) {
    if (!resetTimeStr) return 0;
    const target = new Date(resetTimeStr).getTime();
    if (isNaN(target) || target <= 0) return 0;
    const diff = Math.floor((target - Date.now()) / 1000);
    return Math.max(0, diff);
  }

  function formatRemainingDuration(totalSeconds) {
    if (totalSeconds <= 0) return 'Reset ready';
    const d = Math.floor(totalSeconds / 86400);
    const h = Math.floor((totalSeconds % 86400) / 3600);
    const m = Math.floor((totalSeconds % 3600) / 60);
    const s = totalSeconds % 60;

    if (d > 0) return `${d}d ${h}h ${m}m`;
    if (h > 0) return `${h}h ${m}m ${s}s`;
    if (m > 0) return `${m}m ${s}s`;
    return `${s}s`;
  }

  // =========================================================================
  // Non-Blocking Toast Notification System
  // =========================================================================

  function showToast(message, type = 'info', duration = 4000) {
    if (!toastContainer) return;

    const toast = document.createElement('div');
    toast.className = `toast toast-${type}`;
    toast.setAttribute('role', 'alert');

    toast.innerHTML = `
      <div class="toast-message">${escapeHtml(message)}</div>
      <button class="toast-close" aria-label="Dismiss notification">&times;</button>
    `;

    const closeBtn = toast.querySelector('.toast-close');
    const dismiss = () => {
      toast.style.opacity = '0';
      toast.style.transform = 'translateY(6px)';
      setTimeout(() => {
        if (toast.parentNode) toast.parentNode.removeChild(toast);
      }, 150);
    };

    closeBtn.addEventListener('click', dismiss);
    toastContainer.appendChild(toast);

    if (duration > 0) {
      setTimeout(dismiss, duration);
    }
  }

  async function copyToClipboard(text, label = 'Content') {
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        await navigator.clipboard.writeText(text);
        showToast(`${label} copied to clipboard`, 'success', 2500);
      } else {
        const textarea = document.createElement('textarea');
        textarea.value = text;
        textarea.style.position = 'fixed';
        textarea.style.opacity = '0';
        document.body.appendChild(textarea);
        textarea.select();
        document.execCommand('copy');
        document.body.removeChild(textarea);
        showToast(`${label} copied to clipboard`, 'success', 2500);
      }
    } catch (e) {
      showToast(`Failed to copy to clipboard: ${e.message}`, 'error', 3000);
    }
  }

  // =========================================================================
  // Status & Active Account Management
  // =========================================================================

  async function fetchStatus() {
    try {
      const res = await fetch('/api/status');
      if (!res.ok) return;
      const data = await res.json();

      // Version badge
      const versionEl = document.getElementById('switcher-version');
      if (versionEl && data.version) {
        versionEl.textContent = `v${data.version}`;
      }

      // Active Account Route
      const activeEmailEl = document.getElementById('active-email');
      const activeDetailsEl = document.getElementById('active-details');
      const activeBadge = document.getElementById('active-status-badge');
      const copyActiveBtn = document.getElementById('btn-copy-active-email');

      if (data.active_account) {
        const email = data.active_account.email;
        activeEmailEl.textContent = email;
        activeEmailEl.title = `${email} (Click to copy)`;
        activeEmailEl.style.cursor = 'pointer';
        activeEmailEl.onclick = () => copyToClipboard(email, 'Account email');

        if (copyActiveBtn) {
          copyActiveBtn.style.display = 'inline-flex';
          copyActiveBtn.onclick = () => copyToClipboard(email, 'Account email');
        }

        let createdDateStr = 'recent';
        if (data.active_account.created_at) {
          const d = new Date(data.active_account.created_at);
          if (!isNaN(d.getTime())) {
            createdDateStr = d.toLocaleDateString();
          }
        }
        activeDetailsEl.textContent = `ID: ${data.active_account.id.slice(0, 8)}... • Added ${createdDateStr} • HTTP 429 auto-failover armed`;

        const isExhausted = data.active_account.status === 'exhausted';
        activeBadge.textContent = (data.active_account.status || 'ACTIVE').toUpperCase();
        activeBadge.className = isExhausted ? 'badge badge-danger' : 'badge badge-success';
      } else {
        activeEmailEl.textContent = 'No Active Google Account';
        activeEmailEl.title = 'No account selected';
        activeEmailEl.style.cursor = 'default';
        activeEmailEl.onclick = null;
        if (copyActiveBtn) copyActiveBtn.style.display = 'none';

        activeDetailsEl.textContent = 'Authenticate an account or select one below to arm proxy';
        activeBadge.textContent = 'STANDBY';
        activeBadge.className = 'badge badge-warning';
      }

      // Quick Stats
      const accountsCountStat = document.getElementById('accounts-stat-count');
      const accountsSubtext = document.getElementById('accounts-stat-subtext');
      if (accountsCountStat) {
        accountsCountStat.textContent = data.total_accounts || 0;
      }
      if (accountsSubtext) {
        const standby = Math.max(0, (data.total_accounts || 0) - (data.active_account ? 1 : 0));
        accountsSubtext.textContent = standby > 0 ? `${standby} standby failover target${standby === 1 ? '' : 's'}` : 'Single account routing';
      }
    } catch (e) {
      console.warn('Failed to fetch status:', e);
    }
  }

  // =========================================================================
  // Accounts Pool & Live Quota Matrix
  // =========================================================================

  async function fetchAccounts() {
    try {
      const res = await fetch('/api/accounts');
      if (!res.ok) return;
      const accounts = await res.json();
      rawAccountsData = Array.isArray(accounts) ? accounts : [];

      const countBadge = document.getElementById('accounts-count-badge');
      if (countBadge) {
        countBadge.textContent = `${rawAccountsData.length} account${rawAccountsData.length === 1 ? '' : 's'}`;
      }

      renderAccountsGrid();
    } catch (e) {
      console.warn('Failed to fetch accounts:', e);
    }
  }

  function renderAccountsGrid() {
    if (!accountsGrid) return;
    accountsGrid.innerHTML = '';

    if (rawAccountsData.length === 0) {
      accountsGrid.innerHTML = `
        <div class="empty-state" style="grid-column: 1 / -1;">
          <div class="empty-state-title">No Google Accounts in Pool</div>
          <div class="empty-state-subtext">
            Authenticate your first Google account with 1-Click OAuth to start proxying Google Antigravity 2.0 requests with zero-latency failover.
          </div>
          <button class="btn btn-primary btn-add-first-account" style="margin-top: 0.5rem;">
            Authenticate Account Now
          </button>
        </div>
      `;

      const addFirstBtn = accountsGrid.querySelector('.btn-add-first-account');
      if (addFirstBtn) {
        addFirstBtn.addEventListener('click', handleAddAccount);
      }
      return;
    }

    rawAccountsData.forEach(acc => {
      const card = createAccountCard(acc);
      accountsGrid.appendChild(card);
    });
  }

  function createAccountCard(acc) {
    const card = document.createElement('div');
    const isActive = Boolean(acc.is_active);
    const isExhausted = acc.status === 'exhausted';

    card.className = `account-card ${isActive ? 'is-active' : ''}`;
    card.setAttribute('data-account-id', acc.id);

    const buckets = acc.buckets || [];

    function findBucket(windowPattern, namePattern) {
      return buckets.find(b => {
        const idOrName = ((b.bucket_id || '') + ' ' + (b.display_name || '')).toLowerCase();
        const windowMatch = (b.window || '').toLowerCase().includes(windowPattern.toLowerCase());
        const nameMatch = idOrName.includes(namePattern.toLowerCase());
        return windowMatch && nameMatch;
      }) || null;
    }

    const claude5h = findBucket('5h', '3p') || findBucket('5h', 'claude');
    const claudeWeekly = findBucket('weekly', '3p') || findBucket('weekly', 'claude');
    const gemini5h = findBucket('5h', 'gemini');
    const geminiWeekly = findBucket('weekly', 'gemini');

    // Identify standard buckets to find any custom/other buckets
    const standardBucketSet = new Set([claude5h, claudeWeekly, gemini5h, geminiWeekly].filter(Boolean));
    const extraBuckets = buckets.filter(b => !standardBucketSet.has(b));

    function renderQuotaRow(label, bucket) {
      if (!bucket) {
        return `
          <div class="quota-row">
            <div class="quota-labels">
              <span class="quota-name">${escapeHtml(label)}</span>
              <span class="quota-pct text-muted tabular-nums">--%</span>
            </div>
            <div class="quota-bar-track">
              <div class="quota-bar-fill bar-healthy" style="width: 100%; opacity: 0.15;"></div>
            </div>
          </div>
        `;
      }

      const fraction = Math.max(0, Math.min(1, bucket.remaining_fraction ?? 1.0));
      const pct = Math.round(fraction * 100);
      const isDepleted = pct === 0;

      let barClass = 'bar-healthy';
      let textClass = 'text-success';
      if (pct <= 20) {
        barClass = 'bar-danger';
        textClass = 'text-danger';
      } else if (pct <= 50) {
        barClass = 'bar-warning';
        textClass = 'text-warning';
      }

      let cooldownHtml = '';
      if (isDepleted && bucket.reset_time) {
        const remainingSec = getRemainingSeconds(bucket.reset_time);
        const cooldownStr = formatRemainingDuration(remainingSec);
        const isReady = remainingSec <= 0;
        cooldownHtml = `
          <div class="cooldown-alert" data-reset-time="${escapeHtml(bucket.reset_time)}" style="${isReady ? 'color: var(--color-success);' : ''}">
            <span class="cooldown-dot ${isReady ? 'ready' : ''}"></span>
            <span>Cooldown: <span class="cooldown-timer tabular-nums">${escapeHtml(cooldownStr)}</span></span>
          </div>
        `;
      }

      return `
        <div class="quota-row">
          <div class="quota-labels">
            <span class="quota-name">${escapeHtml(label)}</span>
            <span class="quota-pct ${textClass} tabular-nums">${pct}%</span>
          </div>
          <div class="quota-bar-track">
            <div class="quota-bar-fill ${barClass}" style="width: ${pct}%;"></div>
          </div>
          ${cooldownHtml}
        </div>
      `;
    }

    // Build Model Groups
    let quotaBodyHtml = '';
    if (buckets.length === 0) {
      quotaBodyHtml = `
        <div class="quota-group" style="text-align: center; padding: 0.75rem 0.5rem;">
          <div class="text-xs text-muted">Awaiting quota poll from Google Cloud Code PA...</div>
          <div class="text-xs text-secondary mt-1">Automatic sync occurs every 60s</div>
        </div>
      `;
    } else {
      let extraHtml = '';
      if (extraBuckets.length > 0) {
        extraHtml = `
          <div class="quota-group">
            <div class="quota-group-header">
              <span class="quota-group-name">Additional Quota Limits</span>
              <span class="quota-group-models">${extraBuckets.length} bucket${extraBuckets.length === 1 ? '' : 's'}</span>
            </div>
            ${extraBuckets.map(b => renderQuotaRow(b.display_name || b.bucket_id, b)).join('')}
          </div>
        `;
      }

      quotaBodyHtml = `
        <!-- Group 1: Claude & GPT Models (3P) -->
        <div class="quota-group">
          <div class="quota-group-header">
            <span class="quota-group-name">Claude & GPT Models</span>
            <span class="quota-group-models">Opus, Sonnet, GPT-OSS</span>
          </div>
          ${renderQuotaRow('5-Hour Sliding Limit', claude5h)}
          ${renderQuotaRow('Weekly Sliding Limit', claudeWeekly)}
        </div>

        <!-- Group 2: Gemini Models -->
        <div class="quota-group">
          <div class="quota-group-header">
            <span class="quota-group-name">Gemini Models</span>
            <span class="quota-group-models">Flash, Pro</span>
          </div>
          ${renderQuotaRow('5-Hour Sliding Limit', gemini5h)}
          ${renderQuotaRow('Weekly Sliding Limit', geminiWeekly)}
        </div>

        ${extraHtml}
      `;
    }

    // Status badge formatting
    let statusBadgeClass = 'badge-neutral';
    if (isExhausted) statusBadgeClass = 'badge-danger';
    else if (isActive) statusBadgeClass = 'badge-success';
    else if (acc.status === 'error') statusBadgeClass = 'badge-danger';

    card.innerHTML = `
      <div class="account-header">
        <div class="account-info">
          <div class="account-email" title="${escapeHtml(acc.email)}">
            ${escapeHtml(acc.email)}
          </div>
          <div class="account-id-row">
            <span class="account-id-chip">ID: ${escapeHtml(acc.id.slice(0, 8))}</span>
            <button class="copy-btn btn-copy-email" data-email="${escapeHtml(acc.email)}" title="Copy email address" aria-label="Copy email">
              <svg width="12" height="12" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z" />
              </svg>
            </button>
          </div>
        </div>
        <div class="badge ${statusBadgeClass}">
          ${escapeHtml((acc.status || 'ACTIVE').toUpperCase())}
        </div>
      </div>

      <div class="quota-matrix">
        ${quotaBodyHtml}
      </div>

      <div class="account-actions">
        ${isActive ? `
          <div class="active-pill" aria-current="true">
            Active Routing Target
          </div>
        ` : `
          <button class="btn btn-secondary btn-switch-account" data-id="${escapeHtml(acc.id)}" style="flex: 1;">
            Make Active
          </button>
        `}
        <button class="btn btn-danger-subtle btn-delete-account" data-id="${escapeHtml(acc.id)}" title="Remove Account" aria-label="Remove Account">
          <svg width="15" height="15" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
          </svg>
        </button>
      </div>
    `;

    // Bind copy email
    const copyBtn = card.querySelector('.btn-copy-email');
    if (copyBtn) {
      copyBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        copyToClipboard(acc.email, 'Account email');
      });
    }

    // Bind switch button
    const switchBtn = card.querySelector('.btn-switch-account');
    if (switchBtn) {
      switchBtn.addEventListener('click', () => handleSwitchAccount(acc.id));
    }

    // Bind safe two-step delete button (inline confirmation without window.confirm)
    const deleteBtn = card.querySelector('.btn-delete-account');
    if (deleteBtn) {
      let confirmTimeout = null;
      deleteBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        if (deleteBtn.classList.contains('btn-danger-confirm')) {
          clearTimeout(confirmTimeout);
          handleDeleteAccount(acc.id, acc.email);
        } else {
          deleteBtn.classList.add('btn-danger-confirm');
          deleteBtn.title = 'Click again to confirm removal';
          deleteBtn.innerHTML = `
            <svg width="15" height="15" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2.5" d="M5 13l4 4L19 7" />
            </svg>
          `;
          showToast(`Click the red checkmark to confirm removal of ${acc.email}`, 'info', 3500);

          confirmTimeout = setTimeout(() => {
            deleteBtn.classList.remove('btn-danger-confirm');
            deleteBtn.title = 'Remove Account';
            deleteBtn.innerHTML = `
              <svg width="15" height="15" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
              </svg>
            `;
          }, 3500);
        }
      });
    }

    return card;
  }

  // Live real-time cooldown ticker (ticks every 1s without HTTP requests)
  function updateCooldownTimers() {
    const alerts = document.querySelectorAll('.cooldown-alert[data-reset-time]');
    alerts.forEach(alertEl => {
      const resetTimeStr = alertEl.getAttribute('data-reset-time');
      const timerSpan = alertEl.querySelector('.cooldown-timer');
      const dot = alertEl.querySelector('.cooldown-dot');
      if (timerSpan && resetTimeStr) {
        const remainingSec = getRemainingSeconds(resetTimeStr);
        if (remainingSec <= 0) {
          timerSpan.textContent = 'Reset ready';
          alertEl.style.color = 'var(--color-success)';
          if (dot) dot.classList.add('ready');
        } else {
          timerSpan.textContent = formatRemainingDuration(remainingSec);
          if (dot) dot.classList.remove('ready');
        }
      }
    });
  }

  // =========================================================================
  // Token Metrics & 14-Day Timeline Visualizer (Bug Fix & Redesign)
  // =========================================================================

  async function fetchMetrics() {
    try {
      const res = await fetch(`/api/metrics?period=${encodeURIComponent(currentPeriod)}`);
      if (!res.ok) return;
      const data = await res.json();

      // Quick Stats
      if (data.summary) {
        const todayTokens = data.summary.today?.total_tokens ?? 0;
        const todayReqs = data.summary.today?.total_requests ?? 0;
        const allTimeTokens = data.summary.all_time?.total_tokens ?? 0;
        const allTimeReqs = data.summary.all_time?.total_requests ?? 0;

        const tokensTodayEl = document.getElementById('tokens-today');
        if (tokensTodayEl) {
          tokensTodayEl.textContent = formatCompactNumber(todayTokens);
          tokensTodayEl.title = `${formatNumber(todayTokens)} tokens`;
        }

        const requestsTodayEl = document.getElementById('requests-today');
        if (requestsTodayEl) {
          requestsTodayEl.textContent = `${formatNumber(todayReqs)} requests today`;
        }

        const tokensAllTimeEl = document.getElementById('tokens-alltime');
        if (tokensAllTimeEl) {
          tokensAllTimeEl.textContent = formatCompactNumber(allTimeTokens);
          tokensAllTimeEl.title = `${formatNumber(allTimeTokens)} tokens`;
        }

        const requestsAllTimeEl = document.getElementById('requests-alltime');
        if (requestsAllTimeEl) {
          requestsAllTimeEl.textContent = `${formatNumber(allTimeReqs)} requests all time`;
        }

        // Active period metrics tiles
        let periodSummary = data.summary.all_time;
        if (currentPeriod === 'day') periodSummary = data.summary.today;
        else if (currentPeriod === 'week') periodSummary = data.summary.this_week;
        else if (currentPeriod === 'month') periodSummary = data.summary.this_month;

        if (periodSummary) {
          const promptEl = document.getElementById('metric-prompt');
          if (promptEl) {
            promptEl.textContent = formatCompactNumber(periodSummary.total_prompt_tokens);
            promptEl.title = `${formatNumber(periodSummary.total_prompt_tokens)} prompt tokens`;
          }

          const candEl = document.getElementById('metric-candidates');
          if (candEl) {
            candEl.textContent = formatCompactNumber(periodSummary.total_candidates_tokens);
            candEl.title = `${formatNumber(periodSummary.total_candidates_tokens)} candidates tokens`;
          }

          const cachedEl = document.getElementById('metric-cached');
          if (cachedEl) {
            cachedEl.textContent = formatCompactNumber(periodSummary.total_cached_content_tokens);
            cachedEl.title = `${formatNumber(periodSummary.total_cached_content_tokens)} cached context tokens`;
          }

          const reqsEl = document.getElementById('metric-requests');
          if (reqsEl) {
            reqsEl.textContent = formatNumber(periodSummary.total_requests);
          }
        }
      }

      // Render 14-Day Timeline Chart
      rawTimelineData = Array.isArray(data.timeline) ? data.timeline : [];
      renderTimelineChart(rawTimelineData);
    } catch (e) {
      console.warn('Failed to fetch metrics:', e);
    }
  }

  // Comprehensive Chart & Tooltip Implementation
  function renderTimelineChart(timeline) {
    if (!barsTrack || !chartXAxis) return;

    barsTrack.innerHTML = '';
    chartXAxis.innerHTML = '';
    hideChartTooltip();

    if (!timeline || timeline.length === 0) {
      barsTrack.innerHTML = `
        <div style="width: 100%; height: 100%; display: flex; align-items: center; justify-content: center;" class="text-muted text-xs">
          No requests recorded in this 14-day window.
        </div>
      `;
      if (gridMaxLabel) gridMaxLabel.textContent = '--';
      if (gridMidLabel) gridMidLabel.textContent = '--';
      return;
    }

    const maxTokens = Math.max(10, ...timeline.map(d => d.total_tokens || 0));

    // Update Y-Axis Scale Labels
    if (gridMaxLabel) gridMaxLabel.textContent = formatCompactNumber(maxTokens);
    if (gridMidLabel) gridMidLabel.textContent = formatCompactNumber(Math.round(maxTokens / 2));

    const todayStr = new Date().toISOString().slice(0, 10);

    // Render columns
    timeline.forEach((item, index) => {
      const total = item.total_tokens || 0;
      const prompt = item.prompt_tokens || 0;
      const candidates = item.candidates_tokens || 0;
      const reqs = item.request_count || 0;

      const column = document.createElement('div');
      column.className = 'chart-column';
      column.setAttribute('tabindex', '0');
      column.setAttribute('role', 'button');
      column.setAttribute('data-index', index);
      column.setAttribute('aria-label', `${item.date}: ${formatNumber(total)} tokens across ${reqs} requests`);

      // Height calculation
      let barHeightPct = 0;
      if (total > 0) {
        barHeightPct = Math.max(4, Math.round((total / maxTokens) * 100));
      }

      // Proportional prompt vs candidates inside stacked bar with guaranteed minimum visible slice
      let candidatesHeightPct = 0;
      let promptHeightPct = 100;

      if (total > 0 && candidates > 0) {
        const rawRatio = candidates / total;
        // Ensure candidates has at least 2% height if > 0 so it's discernible
        candidatesHeightPct = Math.max(2, Math.round(rawRatio * 100));
        promptHeightPct = 100 - candidatesHeightPct;
      }

      column.innerHTML = `
        <div class="bar-stack" style="height: ${total === 0 ? '3px' : barHeightPct + '%'}; ${total === 0 ? 'background: var(--border-strong); opacity: 0.35;' : ''}">
          ${total > 0 ? `
            <div class="bar-seg-candidates" style="height: ${candidatesHeightPct}%;"></div>
            <div class="bar-seg-prompt" style="height: ${promptHeightPct}%;"></div>
          ` : ''}
        </div>
      `;

      // Event listeners for hover, touch, click, keyboard
      const onSelect = () => selectChartColumn(index, column, item);
      column.addEventListener('pointerenter', onSelect);
      column.addEventListener('focus', onSelect);
      column.addEventListener('click', onSelect);
      column.addEventListener('keydown', (e) => handleChartKeydown(e, index));

      barsTrack.appendChild(column);

      // X-Axis Date label
      const dateLabel = document.createElement('span');
      dateLabel.className = `x-axis-label ${item.date === todayStr ? 'is-today' : ''}`;
      dateLabel.textContent = formatDateLabel(item.date);
      dateLabel.title = item.date;
      chartXAxis.appendChild(dateLabel);
    });

    // Reset inspection banner
    resetInspector();
  }

  // Handle keyboard navigation between bars
  function handleChartKeydown(e, currentIndex) {
    if (e.key === 'ArrowRight' && currentIndex < rawTimelineData.length - 1) {
      e.preventDefault();
      const nextCol = barsTrack.querySelector(`[data-index="${currentIndex + 1}"]`);
      if (nextCol) nextCol.focus();
    } else if (e.key === 'ArrowLeft' && currentIndex > 0) {
      e.preventDefault();
      const prevCol = barsTrack.querySelector(`[data-index="${currentIndex - 1}"]`);
      if (prevCol) prevCol.focus();
    } else if (e.key === 'Home') {
      e.preventDefault();
      const firstCol = barsTrack.querySelector(`[data-index="0"]`);
      if (firstCol) firstCol.focus();
    } else if (e.key === 'End') {
      e.preventDefault();
      const lastCol = barsTrack.querySelector(`[data-index="${rawTimelineData.length - 1}"]`);
      if (lastCol) lastCol.focus();
    } else if (e.key === 'Escape') {
      hideChartTooltip();
      resetInspector();
    }
  }

  // Select column and display non-clipped tooltip and inspection header
  function selectChartColumn(index, columnEl, item) {
    selectedColumnIndex = index;

    // Highlight active column
    const allCols = barsTrack.querySelectorAll('.chart-column');
    allCols.forEach(col => col.classList.remove('is-selected'));
    columnEl.classList.add('is-selected');

    const total = item.total_tokens || 0;
    const prompt = item.prompt_tokens || 0;
    const candidates = item.candidates_tokens || 0;
    const reqs = item.request_count || 0;

    // 1. Update Inspection Header (Always visible, non-blocking on all devices)
    if (inspectorDate && inspectorData) {
      inspectorDate.textContent = item.date;
      inspectorData.innerHTML = `
        <div class="inspector-chip">
          <span class="text-primary font-bold tabular-nums">${formatNumber(total)}</span>
          <span class="text-muted text-xs">tokens</span>
        </div>
        <div class="inspector-chip">
          <span class="inspector-chip-dot" style="background: var(--chart-prompt);"></span>
          <span class="text-accent tabular-nums">${formatNumber(prompt)}</span>
          <span class="text-muted text-xs">prompt</span>
        </div>
        <div class="inspector-chip">
          <span class="inspector-chip-dot" style="background: var(--chart-candidates);"></span>
          <span class="text-success tabular-nums">${formatNumber(candidates)}</span>
          <span class="text-muted text-xs">candidates</span>
        </div>
        <div class="inspector-chip">
          <span class="text-primary tabular-nums">${formatNumber(reqs)}</span>
          <span class="text-muted text-xs">requests</span>
        </div>
      `;
    }

    // 2. Position Floating Tooltip relative to chartViewport with strict coordinate clamping
    if (chartTooltip && chartViewport) {
      document.getElementById('tooltip-date').textContent = item.date;
      document.getElementById('tooltip-total').textContent = formatNumber(total);
      document.getElementById('tooltip-prompt').textContent = formatNumber(prompt);
      document.getElementById('tooltip-candidates').textContent = formatNumber(candidates);
      document.getElementById('tooltip-reqs').textContent = formatNumber(reqs);

      chartTooltip.classList.add('visible');

      const viewportRect = chartViewport.getBoundingClientRect();
      const colRect = columnEl.getBoundingClientRect();

      // Tooltip is inside chartViewport, so offset relative to viewportRect
      const colCenterX = (colRect.left + colRect.width / 2) - viewportRect.left;
      const tooltipWidth = chartTooltip.offsetWidth || 155;
      const tooltipHeight = chartTooltip.offsetHeight || 85;

      // Strict horizontal clamp within viewport bounds
      const padding = 8;
      const minX = tooltipWidth / 2 + padding;
      const maxX = viewportRect.width - tooltipWidth / 2 - padding;
      const clampedX = Math.max(minX, Math.min(maxX, colCenterX));

      chartTooltip.style.left = `${clampedX}px`;

      // Smart vertical placement:
      // If there is enough room above the bar top, place above the bar
      const barTop = colRect.top - viewportRect.top;
      if (barTop > tooltipHeight + 14) {
        chartTooltip.style.top = `${Math.max(6, barTop - tooltipHeight - 6)}px`;
      } else {
        // Bar is tall: place at top edge smoothly
        chartTooltip.style.top = '8px';
      }
    }
  }

  function hideChartTooltip() {
    if (chartTooltip) {
      chartTooltip.classList.remove('visible');
    }
    const allCols = barsTrack ? barsTrack.querySelectorAll('.chart-column') : [];
    allCols.forEach(col => col.classList.remove('is-selected'));
    selectedColumnIndex = -1;
  }

  function resetInspector() {
    if (inspectorDate && inspectorData) {
      inspectorDate.textContent = '14-Day Daily Usage Trend';
      inspectorData.innerHTML = `
        <span class="text-xs text-muted">Hover or use arrow keys to inspect daily breakdown</span>
      `;
    }
  }

  // Pointer leave listener on chart card
  if (chartCard) {
    chartCard.addEventListener('pointerleave', () => {
      hideChartTooltip();
      resetInspector();
    });
  }

  // =========================================================================
  // Account Actions: Switch & Delete
  // =========================================================================

  async function handleSwitchAccount(accountID) {
    try {
      const res = await fetch(`/api/accounts/${encodeURIComponent(accountID)}/select`, {
        method: 'POST'
      });
      if (res.ok) {
        showToast(`Active proxy account switched to ${accountID.slice(0, 8)}`, 'success');
        await Promise.all([fetchStatus(), fetchAccounts()]);
      } else {
        const err = await res.json().catch(() => ({}));
        const msg = err.error?.message || 'Failed to switch account';
        showToast(msg, 'error');
      }
    } catch (e) {
      showToast(`Network error switching account: ${e.message}`, 'error');
    }
  }

  async function handleDeleteAccount(accountID, email) {
    try {
      const res = await fetch(`/api/accounts/${encodeURIComponent(accountID)}`, {
        method: 'DELETE'
      });
      if (res.ok) {
        showToast(`Account ${email} removed from pool`, 'info');
        await Promise.all([fetchStatus(), fetchAccounts()]);
      } else {
        const err = await res.json().catch(() => ({}));
        showToast(err.error?.message || 'Failed to remove account', 'error');
      }
    } catch (e) {
      showToast(`Network error removing account: ${e.message}`, 'error');
    }
  }

  async function handleAddAccount() {
    const btn = document.getElementById('btn-add-account');
    if (btn) btn.disabled = true;

    try {
      const res = await fetch('/oauth/start', { method: 'POST' });
      const data = await res.json().catch(() => ({}));
      if (res.ok) {
        showToast('OAuth flow initiated. Complete Google sign-in in your browser window.', 'info', 6000);
      } else {
        showToast(data.error?.message || 'Failed to initiate OAuth authorization', 'error');
      }
    } catch (e) {
      showToast(`Error initiating OAuth flow: ${e.message}`, 'error');
    } finally {
      if (btn) setTimeout(() => { btn.disabled = false; }, 1000);
    }
  }

  // =========================================================================
  // Live Proxy Event Logs & Filters
  // =========================================================================

  function appendLog(event) {
    if (!event) return;
    allEvents.push(event);

    // Keep buffer capped at 300 events
    if (allEvents.length > 300) {
      allEvents.shift();
    }

    renderLogEntry(event);
  }

  function renderLogEntry(event) {
    if (!logsContainer) return;

    if (!matchesFilter(event, currentLogFilter)) {
      return;
    }

    const entry = document.createElement('div');
    entry.className = 'log-entry';

    let pillClass = 'pill-info';
    let pillText = (event.type || 'INFO').toUpperCase();

    switch (event.type) {
      case 'failover_429':
        pillClass = 'pill-failover';
        pillText = 'FAILOVER 429';
        break;
      case 'quota_exhausted':
        pillClass = 'pill-exhausted';
        pillText = 'QUOTA EXHAUSTED';
        break;
      case 'quota_restored':
        pillClass = 'pill-restored';
        pillText = 'QUOTA RESTORED';
        break;
      case 'account_switched':
        pillClass = 'pill-switched';
        pillText = 'SWITCH';
        break;
      case 'tokens_captured':
        pillClass = 'pill-tokens';
        pillText = 'TOKENS';
        break;
      case 'error':
        pillClass = 'pill-error';
        pillText = 'ERROR';
        break;
    }

    const timeStr = formatTimeOnly(event.timestamp);
    const accPart = event.account_id ? `<span class="log-account">[${escapeHtml(event.account_id.slice(0, 8))}]</span> ` : '';

    entry.innerHTML = `
      <span class="log-time">${escapeHtml(timeStr)}</span>
      <span class="log-pill ${pillClass}">${escapeHtml(pillText)}</span>
      <span class="log-message">${accPart}${escapeHtml(event.message || '')}</span>
    `;

    logsContainer.appendChild(entry);

    // Trim DOM elements to 200 max
    while (logsContainer.children.length > 200) {
      logsContainer.removeChild(logsContainer.firstChild);
    }

    // Auto-scroll if user has not scrolled up
    if (!isUserScrolledUp) {
      logsContainer.scrollTop = logsContainer.scrollHeight;
    }
  }

  function matchesFilter(event, filter) {
    if (filter === 'all') return true;
    if (filter === 'failover' && event.type === 'failover_429') return true;
    if (filter === 'quota' && (event.type === 'quota_exhausted' || event.type === 'quota_restored')) return true;
    if (filter === 'tokens' && event.type === 'tokens_captured') return true;
    if (filter === 'error' && event.type === 'error') return true;
    return false;
  }

  function reapplyLogFilter() {
    if (!logsContainer) return;
    logsContainer.innerHTML = '';
    allEvents.forEach(evt => renderLogEntry(evt));
    logsContainer.scrollTop = logsContainer.scrollHeight;
  }

  // =========================================================================
  // Server-Sent Events (SSE) Real-Time Stream
  // =========================================================================

  function setupSSE() {
    if (eventSource) {
      eventSource.close();
    }

    const badge = document.getElementById('connection-badge');
    const badgeText = document.getElementById('connection-status-text');
    const badgeDot = document.getElementById('connection-dot');

    try {
      eventSource = new EventSource('/api/events');

      eventSource.onopen = function () {
        if (badge && badgeText && badgeDot) {
          badge.className = 'badge badge-success';
          badgeDot.className = 'status-dot status-dot-success';
          badgeText.textContent = 'Connected';
        }
      };

      eventSource.onerror = function () {
        if (badge && badgeText && badgeDot) {
          badge.className = 'badge badge-warning';
          badgeDot.className = 'status-dot status-dot-warning';
          badgeText.textContent = 'Reconnecting...';
        }
      };

      eventSource.onmessage = function (e) {
        try {
          const event = JSON.parse(e.data);
          appendLog(event);

          if (
            event.type === 'account_switched' ||
            event.type === 'failover_429' ||
            event.type === 'quota_restored' ||
            event.type === 'quota_exhausted'
          ) {
            fetchStatus();
            fetchAccounts();
          } else if (event.type === 'tokens_captured') {
            fetchMetrics();
          }
        } catch (err) {
          console.warn('Failed to parse SSE payload:', err);
        }
      };
    } catch (e) {
      console.warn('EventSource initialization failed:', e);
    }
  }

  // =========================================================================
  // Event Listeners & Boot
  // =========================================================================

  function initListeners() {
    // Sync / Refresh Quotas button
    const btnRefresh = document.getElementById('btn-refresh');
    const refreshIcon = document.getElementById('refresh-icon');

    const triggerRefresh = async () => {
      if (refreshIcon) {
        refreshIcon.style.transition = 'transform 0.5s ease-in-out';
        refreshIcon.style.transform = 'rotate(360deg)';
      }
      try {
        await fetch('/api/quota/refresh', { method: 'POST' });
        showToast('Quota refresh complete', 'info', 2000);
      } catch (e) {
        showToast(`Quota refresh failed: ${e.message}`, 'error');
      } finally {
        setTimeout(() => {
          if (refreshIcon) {
            refreshIcon.style.transition = 'none';
            refreshIcon.style.transform = 'rotate(0deg)';
          }
        }, 500);
      }
      await Promise.all([fetchStatus(), fetchAccounts(), fetchMetrics()]);
    };

    if (btnRefresh) {
      btnRefresh.addEventListener('click', triggerRefresh);
    }

    // Keyboard shortcut: Press R to refresh quotas
    window.addEventListener('keydown', (e) => {
      if ((e.key === 'r' || e.key === 'R') && !e.ctrlKey && !e.metaKey && !e.altKey && !['INPUT', 'TEXTAREA'].includes(document.activeElement?.tagName)) {
        e.preventDefault();
        triggerRefresh();
      }
    });

    // Add account button
    const btnAddAccount = document.getElementById('btn-add-account');
    if (btnAddAccount) {
      btnAddAccount.addEventListener('click', handleAddAccount);
    }

    // Clear logs button
    const btnClearLogs = document.getElementById('btn-clear-logs');
    if (btnClearLogs && logsContainer) {
      btnClearLogs.addEventListener('click', () => {
        allEvents = [];
        logsContainer.innerHTML = `
          <div class="log-entry">
            <span class="log-time">--:--:--</span>
            <span class="log-pill pill-info">SYSTEM</span>
            <span class="log-message text-muted">Logs cleared by user.</span>
          </div>
        `;
        showToast('Console logs cleared', 'info', 2000);
      });
    }

    // Copy all logs button
    const btnCopyLogs = document.getElementById('btn-copy-logs');
    if (btnCopyLogs) {
      btnCopyLogs.addEventListener('click', () => {
        if (allEvents.length === 0) {
          showToast('No logs to copy', 'info', 2000);
          return;
        }
        const textLines = allEvents.map(evt => {
          const t = formatTimeOnly(evt.timestamp);
          const type = (evt.type || 'INFO').toUpperCase();
          const acc = evt.account_id ? `[${evt.account_id.slice(0, 8)}] ` : '';
          return `[${t}] [${type}] ${acc}${evt.message || ''}`;
        }).join('\n');
        copyToClipboard(textLines, 'Proxy logs');
      });
    }

    // Log scroll detection (pause auto-scroll when user inspects history)
    if (logsContainer) {
      logsContainer.addEventListener('scroll', () => {
        const threshold = 30;
        const atBottom = logsContainer.scrollHeight - logsContainer.scrollTop - logsContainer.clientHeight < threshold;
        isUserScrolledUp = !atBottom;
      });
    }

    // Log filter buttons
    const filterButtons = document.querySelectorAll('.filter-btn');
    filterButtons.forEach(btn => {
      btn.addEventListener('click', () => {
        filterButtons.forEach(b => b.classList.remove('active'));
        btn.classList.add('active');
        currentLogFilter = btn.getAttribute('data-filter') || 'all';
        reapplyLogFilter();
      });
    });

    // Period selector buttons
    const periodButtons = document.querySelectorAll('.metrics-tabs .tab-btn');
    periodButtons.forEach(btn => {
      btn.addEventListener('click', () => {
        periodButtons.forEach(b => {
          b.classList.remove('active');
          b.setAttribute('aria-selected', 'false');
        });
        btn.classList.add('active');
        btn.setAttribute('aria-selected', 'true');
        currentPeriod = btn.getAttribute('data-period') || 'lifetime';
        fetchMetrics();
      });
    });
  }

  // Application initialization
  document.addEventListener('DOMContentLoaded', () => {
    initListeners();
    fetchStatus();
    fetchAccounts();
    fetchMetrics();
    setupSSE();

    // Trigger one initial quota synchronization pass
    fetch('/api/quota/refresh', { method: 'POST' })
      .then(() => {
        fetchStatus();
        fetchAccounts();
      })
      .catch(() => {});

    // Live local ticker for quota cooldowns (ticks every 1s, zero network cost)
    setInterval(updateCooldownTimers, 1000);

    // Fallback sync polling every 30 seconds
    setInterval(() => {
      fetchStatus();
      fetchAccounts();
      fetchMetrics();
    }, 30000);
  });
})();
