import { describe, it, expect, beforeEach } from "vitest";
import type { ChatMessage, ChatSession } from "../lib/types";
import {
  chatSessions,
  currentSessionId,
  currentSession,
  ensureSession,
  newSession,
  deleteSession,
  switchToSession,
  updateCurrentSession,
  getCurrentSession,
  getSessionMessages,
} from "./chatHistory";

function get<T>(s: { subscribe: (cb: (v: T) => void) => () => void }): T {
  let val: T;
  s.subscribe((v) => (val = v))();
  return val!;
}

function u(text: string): ChatMessage {
  return { role: "user", content: text };
}
function a(text: string): ChatMessage {
  return { role: "assistant", content: text };
}

beforeEach(async () => {
  localStorage.clear();
  chatSessions.set([]);
  currentSessionId.set("");
  // Wait for store init to settle
  await new Promise((r) => setTimeout(r, 10));
});

describe("chatHistory", () => {
  describe("newSession", () => {
    it("creates a session with generated id", async () => {
      const id = await newSession();
      expect(id).toBeTruthy();
      expect(typeof id).toBe("string");
      const s = getCurrentSession();
      expect(s).not.toBeNull();
      expect(s!.id).toBe(id);
    });

    it("sets default title and empty messages", async () => {
      await newSession();
      const s = getCurrentSession()!;
      expect(s.title).toBe("New chat");
      expect(s.messages).toEqual([]);
    });

    it("sets timestamps", async () => {
      const before = Date.now();
      await newSession();
      const after = Date.now();
      const s = getCurrentSession()!;
      expect(s.createdAt).toBeGreaterThanOrEqual(before);
      expect(s.updatedAt).toBeGreaterThanOrEqual(before);
      expect(s.createdAt).toBeLessThanOrEqual(after);
    });

    it("sets currentSessionId", async () => {
      const id = await newSession();
      // Small delay for async store write
      await new Promise((r) => setTimeout(r, 10));
      expect(get(currentSessionId)).toBe(id);
    });

    it("prepends to sessions list", async () => {
      await newSession();
      const id2 = await newSession();
      expect(get(chatSessions)[0].id).toBe(id2);
    });
  });

  describe("ensureSession", () => {
    it("creates a session when none exist", async () => {
      const id = await ensureSession();
      expect(id).toBeTruthy();
      expect(get(chatSessions).length).toBe(1);
    });

    it("returns existing id when valid", async () => {
      const id = await newSession();
      expect(await ensureSession()).toBe(id);
    });

    it("falls back to first session if current id is stale", async () => {
      await newSession();
      const id2 = await newSession();
      localStorage.setItem("playground-current-session", "nonexistent");
      expect(await ensureSession()).toBe(id2);
    });

    it("no duplicates on repeated calls", async () => {
      await newSession();
      await ensureSession();
      await ensureSession();
      expect(get(chatSessions).length).toBe(1);
    });
  });

  describe("switchToSession", () => {
    it("changes currentSessionId", async () => {
      const id1 = await newSession();
      const id2 = await newSession();
      await switchToSession(id1);
      expect(get(currentSessionId)).toBe(id1);
      await switchToSession(id2);
      expect(get(currentSessionId)).toBe(id2);
    });

    it("updates derived store", async () => {
      const id1 = await newSession();
      await newSession();
      await switchToSession(id1);
      expect(get(currentSession)?.id).toBe(id1);
    });
  });

  describe("updateCurrentSession", () => {
    it("saves messages", async () => {
      await newSession();
      const msgs = [u("hello"), a("hi")];
      await updateCurrentSession(msgs);
      expect(getCurrentSession()!.messages).toEqual(msgs);
    });

    it("derives title from first user msg", async () => {
      await newSession();
      await updateCurrentSession([u("What is life?"), a("42")]);
      expect(getCurrentSession()!.title).toBe("What is life?");
    });

    it("truncates at 50 chars", async () => {
      await newSession();
      const long = "A".repeat(80);
      await updateCurrentSession([u(long), a("ok")]);
      expect(getCurrentSession()!.title).toBe("A".repeat(50) + "…");
    });

    it("empty chat title when no user msgs", async () => {
      await newSession();
      await updateCurrentSession([a("hi")]);
      expect(getCurrentSession()!.title).toBe("Empty chat");
    });

    it("saves model, systemPrompt, temperature", async () => {
      await newSession();
      await updateCurrentSession([u("hi")], "gpt-4", "be helpful", 0.5);
      const s = getCurrentSession()!;
      expect(s.model).toBe("gpt-4");
      expect(s.systemPrompt).toBe("be helpful");
      expect(s.temperature).toBe(0.5);
    });

    it("preserves model when undefined", async () => {
      await newSession();
      await updateCurrentSession([u("hi")], "gemma");
      await updateCurrentSession([u("again")]);
      expect(getCurrentSession()!.model).toBe("gemma");
    });

    it("clears model with empty string", async () => {
      await newSession();
      await updateCurrentSession([u("hi")], "gemma");
      await updateCurrentSession([u("again")], "");
      expect(getCurrentSession()!.model).toBe("");
    });

    it("noop when no current session", async () => {
      await updateCurrentSession([u("hello")]);
      expect(get(chatSessions).length).toBe(0);
    });

    it("persists to localStorage", async () => {
      await newSession();
      await updateCurrentSession([u("persist")]);
      const raw = localStorage.getItem("playground-chat-sessions");
      const sessions: ChatSession[] = JSON.parse(raw!);
      expect(sessions[0].title).toBe("persist");
    });

    it("handles ContentPart[] messages", async () => {
      await newSession();
      const msgs: ChatMessage[] = [
        { role: "user", content: [{ type: "text", text: "analyze" }, { type: "image_url", image_url: { url: "x" } }] },
      ];
      await updateCurrentSession(msgs);
      expect(getCurrentSession()!.title).toBe("analyze");
    });
  });

  describe("deleteSession", () => {
    it("removes from list", async () => {
      const id1 = await newSession();
      const id2 = await newSession();
      await deleteSession(id1);
      expect(get(chatSessions)).toHaveLength(1);
      expect(get(chatSessions)[0].id).toBe(id2);
    });

    it("switches to next when current deleted", async () => {
      const id1 = await newSession();
      const id2 = await newSession();
      await switchToSession(id1);
      await deleteSession(id1);
      expect(get(currentSessionId)).toBe(id2);
    });

    it("auto-creates when last deleted", async () => {
      const id = await newSession();
      await deleteSession(id);
      const sessions = get(chatSessions);
      expect(sessions).toHaveLength(1);
      expect(sessions[0].id).not.toBe(id);
    });

    it("noop for unknown id", async () => {
      await newSession();
      await deleteSession("nonexistent");
      expect(get(chatSessions)).toHaveLength(1);
    });
  });

  describe("getCurrentSession", () => {
    it("null when empty", () => {
      expect(getCurrentSession()).toBeNull();
    });

    it("returns active", async () => {
      const id = await newSession();
      expect(getCurrentSession()!.id).toBe(id);
    });
  });

  describe("getSessionMessages", () => {
    it("[] when empty", () => {
      expect(getSessionMessages()).toEqual([]);
    });

    it("returns messages", async () => {
      await newSession();
      const msgs = [u("q"), a("a")];
      await updateCurrentSession(msgs);
      expect(getSessionMessages()).toEqual(msgs);
    });
  });

  describe("chatSessions store", () => {
    it("starts empty", () => {
      expect(get(chatSessions)).toEqual([]);
    });

    it("holds all sessions", async () => {
      const id1 = await newSession();
      const id2 = await newSession();
      expect(get(chatSessions).map((s) => s.id)).toEqual([id2, id1]);
    });

    it("reactively updates", async () => {
      await newSession();
      await updateCurrentSession([u("hi")]);
      expect(get(chatSessions)[0].messages).toHaveLength(1);
    });
  });

  describe("currentSession derived", () => {
    it("null when no match", () => {
      expect(get(currentSession)).toBeNull();
    });

    it("returns active session", async () => {
      await newSession();
      expect(get(currentSession)!.title).toBe("New chat");
    });

    it("updates on switch", async () => {
      const id2 = await newSession();
      await updateCurrentSession([u("x")]);
      await switchToSession(id2);
      expect(get(currentSession)!.id).toBe(id2);
    });
  });

  describe("multi-session workflow", () => {
    it("preserves state across switches", async () => {
      const id1 = await newSession();
      await updateCurrentSession([u("s1")], "llama3", "brief", 0.3);

      const id2 = await newSession();
      await updateCurrentSession([u("s2")], "gemma", "verbose", 0.8);

      await switchToSession(id1);
      let s = getCurrentSession()!;
      expect(s.model).toBe("llama3");
      expect(s.messages[0].content).toBe("s1");

      await switchToSession(id2);
      s = getCurrentSession()!;
      expect(s.model).toBe("gemma");
      expect(s.messages[0].content).toBe("s2");
    });

    it("20 sessions remain isolated", async () => {
      const ids: string[] = [];
      for (let i = 0; i < 20; i++) {
        ids.push(await newSession());
        await updateCurrentSession([u(`msg ${i}`)]);
      }
      await switchToSession(ids[5]);
      expect(getCurrentSession()!.messages[0].content).toBe("msg 5");
      await switchToSession(ids[0]);
      expect(getCurrentSession()!.messages[0].content).toBe("msg 0");
      await switchToSession(ids[19]);
      expect(getCurrentSession()!.messages[0].content).toBe("msg 19");
    });
  });

  describe("corrupted data", () => {
    it("survives invalid JSON", async () => {
      localStorage.setItem("playground-chat-sessions", "not json{{");
      const id = await newSession();
      expect(id).toBeTruthy();
      expect(get(chatSessions)).toHaveLength(1);
    });
  });
});
