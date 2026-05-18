import { writable } from "svelte/store";
import type { ChatSession } from "../lib/types";

// ---- Storage backend interface ----

export type StorageBackend = "localstorage" | "indexeddb";

export interface ChatStorage {
  readonly backend: StorageBackend;
  loadSessions(): Promise<ChatSession[]>;
  saveSessions(sessions: ChatSession[]): Promise<void>;
  getCurrentId(): Promise<string>;
  setCurrentId(id: string): Promise<void>;
  getUsage(): Promise<{ bytes: number; quota: number }>;
}

// ---- Settings ----

export const storageBackend = (() => {
  const s = writable<StorageBackend>(
    (localStorage.getItem("playground-storage-backend") as StorageBackend) || "localstorage"
  );
  s.subscribe((v) => localStorage.setItem("playground-storage-backend", v));
  return s;
})();

// ---- BroadcastChannel for multi-tab sync ----

const bc = typeof BroadcastChannel !== "undefined"
  ? new BroadcastChannel("llama-swap-chats")
  : null;

export function onRemoteChange(cb: () => void): () => void {
  if (!bc) return () => {};
  const handler = (e: MessageEvent) => {
    if (e.data === "chat-change") cb();
  };
  bc.addEventListener("message", handler);
  return () => bc.removeEventListener("message", handler);
}

function notifyRemote() {
  bc?.postMessage("chat-change");
}

// ---- localStorage backend ----

function lsLoad(): ChatSession[] {
  try {
    const raw = localStorage.getItem("playground-chat-sessions");
    return raw ? JSON.parse(raw) : [];
  } catch {
    return [];
  }
}

function lsSave(sessions: ChatSession[]) {
  const json = JSON.stringify(sessions);
  try {
    localStorage.setItem("playground-chat-sessions", json);
  } catch {
    // Quota exceeded — try to strip images and retry
    const stripped = stripImages(sessions);
    if (stripped !== sessions) {
      try {
        localStorage.setItem("playground-chat-sessions", JSON.stringify(stripped));
      } catch { /* silent fail */ }
    }
  }
}

const localStorageBackend: ChatStorage = {
  backend: "localstorage",
  async loadSessions() { return lsLoad(); },
  async saveSessions(s) { lsSave(s); notifyRemote(); },
  async getCurrentId() {
    return localStorage.getItem("playground-current-session") || "";
  },
  async setCurrentId(id) {
    localStorage.setItem("playground-current-session", id);
    notifyRemote();
  },
  async getUsage() {
    const raw = localStorage.getItem("playground-chat-sessions") || "";
    const all = encodeURIComponent(raw).replace(/%../g, "x").length;
    // localStorage quota is typically 5MB, conservatively report 4.5MB
    return { bytes: all, quota: 4_500_000 };
  },
};

// ---- IndexedDB backend ----

const DB_NAME = "llama-swap-chats";
const DB_VERSION = 1;
const STORE_NAME = "sessions";

function openDB(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, DB_VERSION);
    req.onupgradeneeded = () => {
      if (!req.result.objectStoreNames.contains(STORE_NAME)) {
        req.result.createObjectStore(STORE_NAME);
      }
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

const indexedDBBackend: ChatStorage = {
  backend: "indexeddb",
  async loadSessions() {
    try {
      const db = await openDB();
      const tx = db.transaction(STORE_NAME, "readonly");
      const req = tx.objectStore(STORE_NAME).get("sessions");
      return new Promise((resolve) => {
        req.onsuccess = () => resolve(req.result ? JSON.parse(req.result) : []);
        req.onerror = () => resolve([]);
        tx.oncomplete = () => db.close();
      });
    } catch {
      return [];
    }
  },
  async saveSessions(sessions) {
    try {
      const db = await openDB();
      const tx = db.transaction(STORE_NAME, "readwrite");
      tx.objectStore(STORE_NAME).put(JSON.stringify(sessions), "sessions");
      tx.oncomplete = () => db.close();
      notifyRemote();
    } catch { /* silent fail */ }
  },
  async getCurrentId() {
    try {
      const db = await openDB();
      const tx = db.transaction(STORE_NAME, "readonly");
      const req = tx.objectStore(STORE_NAME).get("currentId");
      return new Promise((resolve) => {
        req.onsuccess = () => resolve(req.result || "");
        req.onerror = () => resolve("");
        tx.oncomplete = () => db.close();
      });
    } catch {
      return "";
    }
  },
  async setCurrentId(id) {
    try {
      const db = await openDB();
      const tx = db.transaction(STORE_NAME, "readwrite");
      tx.objectStore(STORE_NAME).put(id, "currentId");
      tx.oncomplete = () => db.close();
      notifyRemote();
    } catch { /* silent fail */ }
  },
  async getUsage() {
    try {
      const estimate = await navigator.storage?.estimate();
      return {
        bytes: estimate?.usage ?? 0,
        quota: estimate?.quota ?? 50_000_000,
      };
    } catch {
      return { bytes: 0, quota: 50_000_000 };
    }
  },
};

// ---- Active backend ----

let _backend: ChatStorage = localStorageBackend;
let _initialized = false;

function getBackend(): ChatStorage {
  if (_initialized) return _backend;
  _initialized = true;
  const saved = localStorage.getItem("playground-storage-backend") as StorageBackend;
  if (saved === "indexeddb") {
    _backend = indexedDBBackend;
  }
  return _backend;
}

export function switchBackend(backend: StorageBackend) {
  _backend = backend === "indexeddb" ? indexedDBBackend : localStorageBackend;
  storageBackend.set(backend);
}

// ---- Public API used by chatHistory ----

export async function loadSessions(): Promise<ChatSession[]> {
  return getBackend().loadSessions();
}

export async function saveSessions(sessions: ChatSession[]) {
  await getBackend().saveSessions(sessions);
}

export async function getCurrentSessionId(): Promise<string> {
  return getBackend().getCurrentId();
}

export async function setCurrentSessionId(id: string) {
  await getBackend().setCurrentId(id);
}

export async function getStorageUsage(): Promise<{ bytes: number; quota: number }> {
  return getBackend().getUsage();
}

// ---- Image stripping for localstorage safety ----

function stripImages(sessions: ChatSession[]): ChatSession[] {
  return sessions.map((s) => ({
    ...s,
    messages: s.messages.map((m) => {
      if (typeof m.content === "string") return m;
      const textParts = m.content
        .filter((p) => p.type === "text")
        .map((p) => p.text);
      if (textParts.length === m.content.length) return m;
      return {
        ...m,
        content: textParts.join("\n") + "\n[images removed to save space]",
      };
    }),
  }));
}
