<script lang="ts">
  import { onMount } from "svelte";
  import { chatSessions, currentSessionId, newSession, deleteSession, switchToSession } from "../../stores/chatHistory";
  import { storageBackend, switchBackend, getStorageUsage, saveSessions as storageSaveSessions, type StorageBackend } from "../../stores/chatStorage";
  import { Trash2, MessageSquarePlus, ArrowLeftRight, Download, Upload, Settings } from "lucide-svelte";

  let showSidebar = $state(true);
  let confirmDeleteId = $state<string | null>(null);
  let mobileOpen = $state(false);
  let showSettings = $state(false);
  let storageBytes = $state(0);
  let storageQuota = $state(0);
  let usageTimer: ReturnType<typeof setInterval>;
  let fileInput: HTMLInputElement | undefined;

  onMount(() => {
    updateUsage();
    usageTimer = setInterval(updateUsage, 5000);
    return () => clearInterval(usageTimer);
  });

  async function updateUsage() {
    const { bytes, quota } = await getStorageUsage();
    storageBytes = bytes;
    storageQuota = quota;
  }

  function formatBytes(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }

  function usagePercent(): number {
    if (storageQuota === 0) return 0;
    return Math.min(100, (storageBytes / storageQuota) * 100);
  }

  function usageColor(): string {
    const pct = usagePercent();
    if (pct > 80) return "#ef4444";
    if (pct > 60) return "#f59e0b";
    return "#22c55e";
  }

  function formatTime(ts: number): string {
    const d = new Date(ts);
    const now = new Date();
    const isToday = d.toDateString() === now.toDateString();
    const isYesterday = d.toDateString() === new Date(now.setDate(now.getDate() - 1)).toDateString();
    const time = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    if (isToday) return time;
    if (isYesterday) return `Yesterday ${time}`;
    const diffDays = Math.floor((Date.now() - ts) / 86400000);
    if (diffDays < 7) return `${diffDays}d ago ${time}`;
    return d.toLocaleDateString([], { month: "short", day: "numeric" }) + " " + time;
  }

  function handleNewChat() {
    newSession();
    confirmDeleteId = null;
  }

  function handleDelete(id: string) {
    if (confirmDeleteId === id) {
      deleteSession(id);
      confirmDeleteId = null;
    } else {
      confirmDeleteId = id;
      setTimeout(() => {
        if (confirmDeleteId === id) confirmDeleteId = null;
      }, 3000);
    }
  }

  function handleSwitch(id: string) {
    switchToSession(id);
    mobileOpen = false;
  }

  function handleExport() {
    const json = JSON.stringify($chatSessions, null, 2);
    const blob = new Blob([json], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `llama-swap-chats-${new Date().toISOString().slice(0, 10)}.json`;
    a.click();
    URL.revokeObjectURL(url);
  }

  function handleImportClick() {
    fileInput?.click();
  }

  async function handleImport(event: Event) {
    const input = event.target as HTMLInputElement;
    const file = input.files?.[0];
    if (!file) return;
    try {
      const text = await file.text();
      const sessions = JSON.parse(text);
      if (!Array.isArray(sessions)) throw new Error("not an array");
      // Import sessions: merge with existing, avoiding ID conflicts
      const existing = $chatSessions;
      const existingIds = new Set(existing.map((s) => s.id));
      const merged = [...sessions.filter((s) => !existingIds.has(s.id)), ...existing];
      await storageSaveSessions(merged);
      chatSessions.set(merged);
      if (merged.length > 0) {
        switchToSession(merged[0].id);
      }
    } catch {
      alert("Invalid chat export file. Expected JSON array of chat sessions.");
    }
    input.value = "";
  }

  function handleBackendChange(e: Event) {
    const sel = e.target as HTMLSelectElement;
    const backend = sel.value as StorageBackend;
    if (backend === $storageBackend) return;
    if (!confirm(`Switch storage to ${backend}? Current chats will be migrated.`)) {
      sel.value = $storageBackend;
      return;
    }
    // Migrate: save to new backend, then switch
    (async () => {
      const sessions = $chatSessions;
      switchBackend(backend);
      await storageSaveSessions(sessions);
      updateUsage();
      chatSessions.set(sessions);
    })();
  }
</script>

{#if showSidebar}
  <div class="chat-history-sidebar {mobileOpen ? 'open' : ''}">
    {#if mobileOpen}
      <div class="mobile-overlay" onclick={() => mobileOpen = false} role="presentation"></div>
    {/if}

    <div class="sidebar-content">
      <div class="sidebar-header">
        <span class="sidebar-title">Chats</span>
        <div class="header-actions">
          <button class="header-btn" onclick={handleNewChat} title="New chat">
            <MessageSquarePlus size={16} />
          </button>
          <button class="header-btn" onclick={() => showSettings = !showSettings} title="Settings">
            <Settings size={16} />
          </button>
        </div>
      </div>

      <!-- Settings panel -->
      {#if showSettings}
        <div class="settings-panel">
          <div class="setting-row">
            <span class="setting-label">Storage</span>
            <select class="setting-select" value={$storageBackend} onchange={handleBackendChange}>
              <option value="localstorage">localStorage</option>
              <option value="indexeddb">IndexedDB</option>
            </select>
          </div>
          <div class="storage-usage">
            <span class="setting-label">Usage</span>
            <span class="usage-text" style="color: {usageColor()}">
              {formatBytes(storageBytes)} / {formatBytes(storageQuota)}
            </span>
          </div>
          <div class="usage-bar-bg">
            <div class="usage-bar-fill" style="width: {usagePercent()}%; background: {usageColor()}"></div>
          </div>
          <div class="setting-row export-row">
            <button class="export-btn" onclick={handleExport}>
              <Download size={14} /> Export
            </button>
            <button class="export-btn" onclick={handleImportClick}>
              <Upload size={14} /> Import
            </button>
          </div>
          <input type="file" accept=".json" class="hidden-input" bind:this={fileInput} onchange={handleImport} />
        </div>
      {/if}

      <div class="session-list">
        {#each $chatSessions as session (session.id)}
          <div
            class="session-item {$currentSessionId === session.id ? 'active' : ''}"
            onclick={() => handleSwitch(session.id)}
            role="button"
            tabindex="0"
            onkeydown={(e) => e.key === 'Enter' && handleSwitch(session.id)}
          >
            <div class="session-info">
              <div class="session-title">{session.title}</div>
              <div class="session-meta">
                <span class="session-time">{formatTime(session.updatedAt)}</span>
                {#if session.messages.length > 0}
                  <span class="session-count">{session.messages.length} msgs</span>
                {/if}
              </div>
            </div>
            <button
              class="delete-btn {confirmDeleteId === session.id ? 'confirm' : ''}"
              onclick={(e) => { e.stopPropagation(); handleDelete(session.id); }}
              title={confirmDeleteId === session.id ? 'Click again to confirm' : 'Delete chat'}
            >
              <Trash2 size={14} />
            </button>
          </div>
        {:else}
          <div class="empty-state">No chats yet. Start a new one!</div>
        {/each}
      </div>
    </div>

    <button class="toggle-btn" onclick={() => mobileOpen = !mobileOpen} title="Toggle sidebar">
      <ArrowLeftRight size={14} />
    </button>
  </div>
{/if}

<style>
  .chat-history-sidebar {
    width: 260px;
    min-width: 260px;
    border-right: 1px solid var(--border-color, rgba(255,255,255,0.1));
    background: var(--surface, #f9fafb);
    display: flex;
    flex-direction: column;
    height: 100%;
    transition: width 0.2s;
  }
  .sidebar-content {
    display: flex;
    flex-direction: column;
    height: 100%;
  }
  .sidebar-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 12px 12px 8px;
    border-bottom: 1px solid var(--border-color, rgba(255,255,255,0.05));
    flex-shrink: 0;
  }
  .sidebar-title {
    font-size: 14px;
    font-weight: 600;
    color: var(--txtcolor, #1f2937);
  }
  .header-actions {
    display: flex;
    gap: 4px;
  }
  .header-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 30px;
    height: 30px;
    border-radius: 6px;
    border: none;
    background: transparent;
    color: var(--txtsecondary, #6b7280);
    cursor: pointer;
    transition: background 0.15s, color 0.15s;
  }
  .header-btn:hover {
    background: var(--hover, rgba(0,0,0,0.05));
    color: var(--txtcolor, #1f2937);
  }
  .settings-panel {
    padding: 10px 12px;
    border-bottom: 1px solid var(--border-color, rgba(255,255,255,0.05));
    flex-shrink: 0;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .setting-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }
  .setting-label {
    font-size: 12px;
    color: var(--txtsecondary, #9ca3af);
  }
  .setting-select {
    font-size: 12px;
    padding: 3px 6px;
    border-radius: 4px;
    border: 1px solid var(--border-color, rgba(255,255,255,0.1));
    background: var(--card, white);
    color: var(--txtcolor, #1f2937);
    cursor: pointer;
  }
  .storage-usage {
    display: flex;
    justify-content: space-between;
  }
  .usage-text {
    font-size: 11px;
    font-family: monospace;
  }
  .usage-bar-bg {
    height: 4px;
    border-radius: 2px;
    background: var(--hover, rgba(0,0,0,0.1));
    overflow: hidden;
  }
  .usage-bar-fill {
    height: 100%;
    border-radius: 2px;
    transition: width 0.3s, background 0.3s;
  }
  .export-row {
    gap: 6px;
  }
  .export-btn {
    display: flex;
    align-items: center;
    gap: 4px;
    font-size: 11px;
    padding: 5px 10px;
    border-radius: 6px;
    border: 1px solid var(--border-color, rgba(255,255,255,0.1));
    background: var(--card, white);
    color: var(--txtsecondary, #6b7280);
    cursor: pointer;
    transition: color 0.15s, background 0.15s;
  }
  .export-btn:hover {
    color: var(--txtcolor, #1f2937);
    background: var(--hover, rgba(0,0,0,0.05));
  }
  .hidden-input {
    display: none;
  }
  .session-list {
    flex: 1;
    overflow-y: auto;
    padding: 4px 8px;
  }
  .session-item {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 8px 8px;
    border-radius: 8px;
    cursor: pointer;
    transition: background 0.15s;
    margin-bottom: 2px;
    user-select: none;
  }
  .session-item:hover {
    background: var(--hover, rgba(0,0,0,0.05));
  }
  .session-item.active {
    background: var(--primary-light, rgba(59,130,246,0.1));
  }
  .session-info {
    flex: 1;
    min-width: 0;
    overflow: hidden;
  }
  .session-title {
    font-size: 13px;
    font-weight: 500;
    color: var(--txtcolor, #1f2937);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    line-height: 1.3;
  }
  .session-meta {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-top: 2px;
  }
  .session-time {
    font-size: 11px;
    color: var(--txtsecondary, #9ca3af);
  }
  .session-count {
    font-size: 10px;
    color: var(--txtsecondary, #9ca3af);
    background: var(--hover, rgba(0,0,0,0.05));
    padding: 1px 5px;
    border-radius: 4px;
  }
  .delete-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 28px;
    height: 28px;
    border-radius: 6px;
    border: none;
    background: transparent;
    color: var(--txtsecondary, #9ca3af);
    cursor: pointer;
    opacity: 0;
    transition: opacity 0.15s, background 0.15s, color 0.15s;
    flex-shrink: 0;
    margin-left: 4px;
  }
  .session-item:hover .delete-btn {
    opacity: 1;
  }
  .delete-btn:hover {
    background: rgba(239,68,68,0.1);
    color: #ef4444;
  }
  .delete-btn.confirm {
    opacity: 1;
    background: rgba(239,68,68,0.15);
    color: #ef4444;
  }
  .empty-state {
    text-align: center;
    color: var(--txtmuted, #d1d5db);
    padding: 24px 8px;
    font-size: 13px;
  }
  .toggle-btn {
    display: none;
    position: fixed;
    right: 12px;
    top: 50%;
    transform: translateY(-50%);
    z-index: 10;
    background: var(--surface, #f9fafb);
    border: 1px solid var(--border-color, rgba(255,255,255,0.1));
    border-radius: 4px;
    padding: 6px;
    cursor: pointer;
    color: var(--txtsecondary, #6b7280);
  }
  .mobile-overlay {
    display: none;
    position: fixed;
    inset: 0;
    background: rgba(0,0,0,0.4);
    z-index: 45;
  }

  @media (max-width: 768px) {
    .chat-history-sidebar {
      position: fixed;
      left: -280px;
      top: 0;
      bottom: 0;
      z-index: 50;
      width: 280px;
      min-width: 0;
      transition: left 0.25s;
      box-shadow: 2px 0 12px rgba(0,0,0,0.15);
    }
    .chat-history-sidebar.open {
      left: 0;
    }
    .chat-history-sidebar.open .mobile-overlay {
      display: block;
    }
    .toggle-btn {
      display: flex;
    }
  }
</style>
