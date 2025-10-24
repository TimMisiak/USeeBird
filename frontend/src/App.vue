<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'

type MessageType = 'chat' | 'system' | 'ping'

interface ChatLogEntry {
  id: string
  type: MessageType
  text: string
  timestamp: Date
  sender?: string
  latencyMs?: number
  pending?: boolean
}

interface ServerMessage {
  type: MessageType
  text?: string
  id?: string
  sentAt?: string
  serverTime?: string
  sender?: string
}

const connectionStatus = ref<'connecting' | 'connected' | 'disconnected'>('connecting')
const statusText = computed(() => {
  switch (connectionStatus.value) {
    case 'connected':
      return 'Connected'
    case 'connecting':
      return 'Connecting…'
    default:
      return 'Disconnected'
  }
})

const ws = ref<WebSocket | null>(null)
const shouldReconnect = ref(true)
let reconnectHandle: ReturnType<typeof setTimeout> | null = null

const messages = ref<ChatLogEntry[]>([])
const messageInput = ref('')
const logContainer = ref<HTMLDivElement | null>(null)
const selfId = ref<string | null>(null)

const pendingPings = new Map<
  string,
  { startedAt: number; messageIndex: number }
>()

const canSend = computed(
  () => connectionStatus.value === 'connected' && ws.value?.readyState === WebSocket.OPEN,
)

function formatTime(value: Date) {
  return value.toLocaleTimeString()
}

function shortId(id?: string) {
  if (!id) return 'unknown'
  return id.slice(0, 8)
}

function appendMessage(entry: ChatLogEntry) {
  messages.value.push(entry)
}

watch(
  messages,
  () => {
    nextTick(() => {
      const el = logContainer.value
      if (el) {
        el.scrollTop = el.scrollHeight
      }
    })
  },
  { deep: true },
)

function scheduleReconnect() {
  if (!shouldReconnect.value) {
    return
  }
  if (reconnectHandle) {
    return
  }
  reconnectHandle = setTimeout(() => {
    reconnectHandle = null
    connect()
  }, 1500)
}

function connect() {
  if (ws.value && (ws.value.readyState === WebSocket.OPEN || ws.value.readyState === WebSocket.CONNECTING)) {
    return
  }

  connectionStatus.value = 'connecting'
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const socket = new WebSocket(`${protocol}//${window.location.host}/ws`)
  ws.value = socket

  socket.addEventListener('open', () => {
    connectionStatus.value = 'connected'
    appendMessage({
      id: crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
      type: 'system',
      text: 'Connected to chat server.',
      timestamp: new Date(),
    })
  })

  socket.addEventListener('close', () => {
    connectionStatus.value = 'disconnected'
    ws.value = null
    appendMessage({
      id: crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
      type: 'system',
      text: 'Connection closed.',
      timestamp: new Date(),
    })
    pendingPings.forEach((tracker) => {
      const entry = messages.value[tracker.messageIndex]
      if (entry && entry.pending) {
        messages.value[tracker.messageIndex] = {
          ...entry,
          pending: false,
          text: 'Ping canceled — connection closed before acknowledgement.',
        }
      }
    })
    pendingPings.clear()
    scheduleReconnect()
  })

  socket.addEventListener('error', () => {
    appendMessage({
      id: crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
      type: 'system',
      text: 'WebSocket error encountered.',
      timestamp: new Date(),
    })
  })

  socket.addEventListener('message', (event) => {
    handleServerMessage(event.data)
  })
}

function handleServerMessage(raw: string) {
  let parsed: ServerMessage
  try {
    parsed = JSON.parse(raw)
  } catch (error) {
    appendMessage({
      id: crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
      type: 'system',
      text: 'Received malformed message from server.',
      timestamp: new Date(),
    })
    return
  }

  const timestamp = parsed.serverTime ? new Date(parsed.serverTime) : new Date()

  switch (parsed.type) {
    case 'system':
      if (parsed.text === 'connected' && parsed.sender) {
        selfId.value = parsed.sender
        appendMessage({
          id: parsed.id ?? crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
          type: 'system',
          text: `Session established. Your client id is ${shortId(parsed.sender)}.`,
          timestamp,
          sender: parsed.sender,
        })
      } else if (parsed.text) {
        appendMessage({
          id: parsed.id ?? crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
          type: 'system',
          text: parsed.text,
          timestamp,
          sender: parsed.sender,
        })
      }
      break
    case 'chat':
      if (!parsed.text) {
        return
      }
      appendMessage({
        id: parsed.id ?? crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
        type: 'chat',
        text: parsed.text,
        timestamp,
        sender: parsed.sender,
      })
      break
    case 'ping':
      handleIncomingPing(parsed, timestamp)
      break
  }
}

function handleIncomingPing(message: ServerMessage, timestamp: Date) {
  if (!message.id) {
    return
  }

  const tracker = pendingPings.get(message.id)
  if (tracker) {
    const latency = performance.now() - tracker.startedAt
    const entry = messages.value[tracker.messageIndex]
    if (entry) {
      messages.value[tracker.messageIndex] = {
        ...entry,
        pending: false,
        latencyMs: latency,
        timestamp,
        text: `Ping acknowledged by server.`,
      }
    }
    pendingPings.delete(message.id)
    return
  }

  appendMessage({
    id: message.id,
    type: 'ping',
    text: `Ping broadcast received from ${shortId(message.sender)}.`,
    timestamp,
    sender: message.sender,
    pending: false,
  })
}

function sendChat() {
  const text = messageInput.value.trim()
  if (!text || !ws.value || ws.value.readyState !== WebSocket.OPEN) {
    return
  }

  const payload = {
    type: 'chat',
    text,
    id: crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
    sentAt: new Date().toISOString(),
  }
  ws.value.send(JSON.stringify(payload))
  messageInput.value = ''
}

function sendPing() {
  if (!ws.value || ws.value.readyState !== WebSocket.OPEN) {
    return
  }
  const id = crypto.randomUUID?.() ?? Math.random().toString(36).slice(2)
  const entry: ChatLogEntry = {
    id,
    type: 'ping',
    text: 'Ping sent to server…',
    timestamp: new Date(),
    sender: selfId.value ?? undefined,
    pending: true,
  }
  appendMessage(entry)
  pendingPings.set(id, { startedAt: performance.now(), messageIndex: messages.value.length - 1 })

  const payload = {
    type: 'ping',
    id,
    sentAt: new Date().toISOString(),
  }
  ws.value.send(JSON.stringify(payload))
}

function handleSubmit(event: Event) {
  event.preventDefault()
  sendChat()
}

onMounted(() => {
  connect()
})

onBeforeUnmount(() => {
  shouldReconnect.value = false
  if (reconnectHandle) {
    clearTimeout(reconnectHandle)
    reconnectHandle = null
  }
  if (ws.value) {
    ws.value.close()
  }
})
</script>

<template>
  <div class="app-shell">
    <header class="header">
      <h1>Realtime Chat</h1>
      <div class="status" :data-status="connectionStatus">
        <span class="indicator"></span>
        <span>{{ statusText }}</span>
      </div>
    </header>

    <section class="log" ref="logContainer">
      <template v-if="messages.length">
        <article
          v-for="entry in messages"
          :key="entry.id"
          class="message"
          :data-type="entry.type"
          :data-self="entry.sender && entry.sender === selfId ? 'yes' : 'no'"
        >
          <header class="message-meta">
            <span class="time">{{ formatTime(entry.timestamp) }}</span>
            <span v-if="entry.type !== 'system'" class="sender">
              {{ entry.sender && entry.sender === selfId ? 'You' : shortId(entry.sender) }}
            </span>
            <span v-if="entry.type === 'ping'" class="ping-state" :data-pending="entry.pending ? 'yes' : 'no'">
              {{ entry.pending ? 'waiting…' : entry.latencyMs !== undefined ? `${entry.latencyMs.toFixed(2)} ms` : 'received' }}
            </span>
          </header>
          <p class="message-text">{{ entry.text }}</p>
          <p v-if="entry.type === 'ping' && entry.latencyMs !== undefined" class="latency">
            Round trip latency: {{ entry.latencyMs.toFixed(2) }} ms
          </p>
        </article>
      </template>
      <p v-else class="placeholder">Waiting for messages…</p>
    </section>

    <form class="input-bar" @submit="handleSubmit">
      <input
        v-model="messageInput"
        type="text"
        placeholder="Type a message and press Enter"
        :disabled="!canSend"
      >
      <button type="submit" :disabled="!canSend || !messageInput.trim()">Send</button>
      <button type="button" class="ping" :disabled="!canSend" @click="sendPing">Ping</button>
    </form>
  </div>
</template>

<style scoped>
.app-shell {
  min-height: 100vh;
  display: flex;
  flex-direction: column;
  gap: 1rem;
  padding: 2rem;
  color: #f1f5f9;
  font-family: 'Inter', system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
}

.header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 1rem;
}

.header h1 {
  font-size: 1.5rem;
  font-weight: 600;
  margin: 0;
}

.status {
  display: inline-flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0.35rem 0.75rem;
  border-radius: 999px;
  background-color: rgba(148, 163, 184, 0.15);
  font-size: 0.9rem;
  text-transform: uppercase;
  letter-spacing: 0.05em;
}

.status .indicator {
  width: 0.75rem;
  height: 0.75rem;
  border-radius: 50%;
  background: #fbbf24;
  box-shadow: 0 0 8px rgba(251, 191, 36, 0.6);
  transition: background-color 0.3s ease, box-shadow 0.3s ease;
}

.status[data-status='connected'] .indicator {
  background: #34d399;
  box-shadow: 0 0 8px rgba(52, 211, 153, 0.7);
}

.status[data-status='disconnected'] .indicator {
  background: #ef4444;
  box-shadow: 0 0 8px rgba(239, 68, 68, 0.6);
}

.log {
  flex: 1;
  overflow-y: auto;
  padding: 1rem;
  background: rgba(15, 23, 42, 0.6);
  border-radius: 1rem;
  box-shadow: inset 0 0 0 1px rgba(148, 163, 184, 0.08);
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}

.placeholder {
  text-align: center;
  color: rgba(148, 163, 184, 0.7);
  margin: auto 0;
}

.message {
  padding: 0.75rem 1rem;
  border-radius: 0.75rem;
  background: rgba(30, 41, 59, 0.75);
  box-shadow: 0 2px 8px rgba(15, 23, 42, 0.2);
  backdrop-filter: blur(6px);
}

.message[data-type='system'] {
  background: rgba(148, 163, 184, 0.2);
  color: #e2e8f0;
}

.message[data-type='chat'][data-self='yes'] {
  background: linear-gradient(135deg, rgba(56, 189, 248, 0.8), rgba(99, 102, 241, 0.8));
}

.message-meta {
  display: flex;
  align-items: center;
  gap: 0.75rem;
  font-size: 0.8rem;
  color: rgba(226, 232, 240, 0.8);
  margin-bottom: 0.35rem;
}

.message-text {
  margin: 0;
  font-size: 1rem;
  line-height: 1.4;
}

.latency {
  margin: 0.35rem 0 0;
  font-size: 0.85rem;
  color: rgba(226, 232, 240, 0.75);
}

.sender {
  font-weight: 600;
}

.ping-state {
  margin-left: auto;
  padding: 0.25rem 0.5rem;
  border-radius: 999px;
  background: rgba(148, 163, 184, 0.18);
  font-weight: 600;
}

.ping-state[data-pending='yes'] {
  background: rgba(251, 191, 36, 0.22);
  color: #fbbf24;
}

.input-bar {
  display: grid;
  grid-template-columns: 1fr auto auto;
  gap: 0.75rem;
  align-items: center;
}

.input-bar input {
  padding: 0.75rem 1rem;
  border-radius: 0.75rem;
  border: none;
  background: rgba(30, 41, 59, 0.85);
  color: #f8fafc;
  font-size: 1rem;
}

.input-bar input:disabled {
  opacity: 0.5;
}

.input-bar button {
  padding: 0.75rem 1.25rem;
  border-radius: 0.75rem;
  border: none;
  font-weight: 600;
  cursor: pointer;
  transition: transform 0.1s ease, box-shadow 0.2s ease, opacity 0.2s ease;
}

.input-bar button[disabled] {
  cursor: not-allowed;
  opacity: 0.5;
  box-shadow: none;
}

.input-bar button:not([disabled]):active {
  transform: translateY(1px);
}

.input-bar button[type='submit'] {
  background: linear-gradient(135deg, #38bdf8, #6366f1);
  color: #0f172a;
  box-shadow: 0 10px 25px rgba(59, 130, 246, 0.35);
}

.input-bar button.ping {
  background: rgba(248, 250, 252, 0.15);
  color: #e2e8f0;
  box-shadow: 0 10px 25px rgba(226, 232, 240, 0.18);
}

@media (max-width: 640px) {
  .app-shell {
    padding: 1.5rem;
  }

  .input-bar {
    grid-template-columns: 1fr;
  }

  .input-bar button {
    width: 100%;
  }
}
</style>
