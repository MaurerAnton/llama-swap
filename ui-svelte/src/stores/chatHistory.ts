import { derived, writable } from "svelte/store";
import type { ChatSession, ChatMessage } from "../lib/types";
import { getTextContent } from "../lib/types";
import {
  loadSessions,
  saveSessions,
  getCurrentSessionId,
  setCurrentSessionId,
  onRemoteChange,
} from "./chatStorage";

function generateId(): string {
  return Date.now().toString(36) + Math.random().toString(36).slice(2, 8);
}

function deriveTitle(messages: ChatMessage[]): string {
  const firstUserMsg = messages.find((m) => m.role === "user");
  if (!firstUserMsg) return "Empty chat";
  const text = getTextContent(firstUserMsg.content);
  return text.slice(0, 50) + (text.length > 50 ? "…" : "");
}

export const chatSessions = writable<ChatSession[]>([]);

async function initStore() {
  chatSessions.set(await loadSessions());
}
initStore();

// Listen for remote tab changes
onRemoteChange(async () => {
  chatSessions.set(await loadSessions());
});

// Ensure there's always at least one session
export async function ensureSession(): Promise<string> {
  const sessions = await loadSessions();
  if (sessions.length > 0) {
    const currentId = await getCurrentSessionId();
    if (currentId && sessions.some((s) => s.id === currentId)) {
      return currentId;
    }
    const id = sessions[0].id;
    await setCurrentSessionId(id);
    return id;
  }
  return newSession();
}

export async function newSession(): Promise<string> {
  const id = generateId();
  const session: ChatSession = {
    id,
    title: "New chat",
    createdAt: Date.now(),
    updatedAt: Date.now(),
    messages: [],
  };
  const sessions = await loadSessions();
  sessions.unshift(session);
  await saveSessions(sessions);
  await setCurrentSessionId(id);
  chatSessions.set(sessions);
  if (_sid) _sid.set(id);
  return id;
}

export async function deleteSession(id: string) {
  let sessions = await loadSessions();
  sessions = sessions.filter((s) => s.id !== id);

  const currentId = await getCurrentSessionId();
  if (currentId === id) {
    if (sessions.length > 0) {
      const newCurrentId = sessions[0].id;
      await setCurrentSessionId(newCurrentId);
      if (_sid) _sid.set(newCurrentId);
    } else {
      await saveSessions(sessions);
      await newSession();
      return;
    }
  }

  await saveSessions(sessions);
  chatSessions.set(sessions);
}

export async function switchToSession(id: string) {
  await setCurrentSessionId(id);
  if (_sid) _sid.set(id);
  chatSessions.update((s) => [...s]);
}

export async function updateCurrentSession(
	messages: ChatMessage[],
	model?: string,
	systemPrompt?: string,
	temperature?: number
) {
	const currentId = await getCurrentSessionId();
	if (!currentId) return;
	await saveSessionById(currentId, messages, model, systemPrompt, temperature);
}

async function saveSessionById(
	sessionId: string,
	messages: ChatMessage[],
	model?: string,
	systemPrompt?: string,
	temperature?: number
) {
	const sessions = await loadSessions();
	const idx = sessions.findIndex((s) => s.id === sessionId);
	if (idx === -1) return;

	sessions[idx] = {
		...sessions[idx],
		title: deriveTitle(messages),
		updatedAt: Date.now(),
		messages,
		...(model !== undefined ? { model } : {}),
		...(systemPrompt !== undefined ? { systemPrompt } : {}),
		...(temperature !== undefined ? { temperature } : {}),
	};

	await saveSessions(sessions);
	chatSessions.set(sessions);
}

export { saveSessionById };

export function getCurrentSession(): ChatSession | null {
  let currentId = "";
  let sessions: ChatSession[] = [];
  chatSessions.subscribe((v) => (sessions = v))();
  currentSessionId.subscribe((v) => (currentId = v))();
  return sessions.find((s) => s.id === currentId) ?? null;
}

export function getSessionMessages(): ChatMessage[] {
  return getCurrentSession()?.messages ?? [];
}

// Lazy-initialized store to avoid TDZ during module init
let _sid: ReturnType<typeof writable<string>>;

export const currentSessionId = {
  subscribe(run: (value: string) => void) {
    if (!_sid) {
      _sid = writable("");
      ensureSession().then((id) => {
        if (_sid) _sid.set(id);
      });
    }
    return _sid.subscribe(run);
  },
  set(value: string) {
    if (!_sid) _sid = writable(value);
    else _sid.set(value);
  },
  update(fn: (value: string) => string) {
    if (!_sid) _sid = writable(fn(""));
    else _sid.update(fn);
  },
};

export const currentSession = derived(
  [chatSessions, currentSessionId],
  ([$sessions, $id]) => $sessions.find((s) => s.id === $id) ?? null
);
