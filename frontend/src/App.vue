<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'

type MessageType = 'chat' | 'system' | 'ping'
type SignalingMessageType =
  | MessageType
  | 'webrtc-offer'
  | 'webrtc-answer'
  | 'webrtc-ice'
  | 'webrtc-presence'
  | 'webrtc-presence-request'

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
  type: SignalingMessageType
  text?: string
  id?: string
  sentAt?: string
  serverTime?: string
  sender?: string
  target?: string
  sdp?: string
  candidate?: string
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

const webrtcEnabled = ref(false)
const peerConnections = new Map<string, RTCPeerConnection>()
const dataChannels = new Map<string, RTCDataChannel>()
const webrtcPendingPings = new Map<
  string,
  { startedAt: number; messageIndex: number; peerId: string }
>()
const knownPeers = ref<string[]>([])

const canSend = computed(
  () => connectionStatus.value === 'connected' && ws.value?.readyState === WebSocket.OPEN,
)
const canSendWebRTCPing = computed(() => canSend.value)

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

function addKnownPeer(peerId?: string | null) {
  if (!peerId || peerId === selfId.value) {
    return
  }
  if (!knownPeers.value.includes(peerId)) {
    knownPeers.value = [...knownPeers.value, peerId]
  }
  if (webrtcEnabled.value) {
    void initiateConnectionWith(peerId)
  }
}

function sendSignalingMessage(message: Record<string, unknown> & { type: SignalingMessageType }) {
  if (!ws.value || ws.value.readyState !== WebSocket.OPEN) {
    return
  }
  ws.value.send(JSON.stringify(message))
}

function requestWebRTCPresence() {
  if (!ws.value || ws.value.readyState !== WebSocket.OPEN) {
    return
  }
  sendSignalingMessage({
    type: 'webrtc-presence-request',
    id: crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
    sentAt: new Date().toISOString(),
  })
}

function ensureWebRTCSetup() {
  if (!webrtcEnabled.value) {
    webrtcEnabled.value = true
    appendMessage({
      id: crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
      type: 'system',
      text: 'Attempting to establish WebRTC connections…',
      timestamp: new Date(),
    })
  }
  requestWebRTCPresence()
  knownPeers.value.forEach((peerId) => {
    void initiateConnectionWith(peerId)
  })
}

function cleanupPeer(peerId: string) {
  const channel = dataChannels.get(peerId)
  if (channel) {
    channel.onclose = null
    channel.onmessage = null
    channel.onopen = null
    if (channel.readyState === 'open') {
      channel.close()
    }
    dataChannels.delete(peerId)
  }
  const pc = peerConnections.get(peerId)
  if (pc) {
    pc.onicecandidate = null
    pc.onconnectionstatechange = null
    pc.ondatachannel = null
    pc.close()
    peerConnections.delete(peerId)
  }
}

function handleDataChannelMessage(peerId: string, raw: string | ArrayBuffer | Blob) {
  if (raw instanceof ArrayBuffer || raw instanceof Blob) {
    return
  }
  let parsed: any
  try {
    parsed = JSON.parse(raw)
  } catch (error) {
    console.warn('Received malformed WebRTC data channel message', error)
    return
  }

  if (parsed?.type === 'webrtc-ping') {
    if (parsed.ack && typeof parsed.id === 'string') {
      const tracker = webrtcPendingPings.get(parsed.id)
      if (tracker) {
        const entry = messages.value[tracker.messageIndex]
        if (entry) {
          messages.value[tracker.messageIndex] = {
            ...entry,
            pending: false,
            latencyMs: performance.now() - tracker.startedAt,
            timestamp: new Date(),
            text: `WebRTC ping acknowledged by ${shortId(peerId)}.`,
          }
        }
        webrtcPendingPings.delete(parsed.id)
      }
      return
    }

    const channel = dataChannels.get(peerId)
    if (channel && channel.readyState === 'open') {
      const response = {
        type: 'webrtc-ping',
        id:
          typeof parsed.id === 'string'
            ? parsed.id
            : crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
        ack: true,
        sentAt: parsed.sentAt,
      }
      channel.send(JSON.stringify(response))
    }

    appendMessage({
      id:
        typeof parsed.id === 'string'
          ? parsed.id
          : crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
      type: 'ping',
      text: `WebRTC ping received from ${shortId(peerId)}.`,
      timestamp: new Date(),
      sender: peerId,
      pending: false,
    })
  }
}

function setupDataChannel(peerId: string, channel: RTCDataChannel) {
  dataChannels.set(peerId, channel)
  channel.onopen = () => {
    appendMessage({
      id: crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
      type: 'system',
      text: `WebRTC data channel opened with ${shortId(peerId)}.`,
      timestamp: new Date(),
    })
  }
  channel.onclose = () => {
    appendMessage({
      id: crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
      type: 'system',
      text: `WebRTC data channel closed with ${shortId(peerId)}.`,
      timestamp: new Date(),
    })
    dataChannels.delete(peerId)
  }
  channel.onmessage = (event) => {
    handleDataChannelMessage(peerId, event.data)
  }
}

function createPeerConnection(peerId: string) {
  const pc = new RTCPeerConnection({
    iceServers: [{ urls: 'stun:stun.l.google.com:19302' }],
  })
  pc.onicecandidate = (event) => {
    if (event.candidate) {
      sendSignalingMessage({
        type: 'webrtc-ice',
        target: peerId,
        candidate: JSON.stringify(event.candidate),
      })
    }
  }
  pc.onconnectionstatechange = () => {
    if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
      cleanupPeer(peerId)
    }
  }
  pc.ondatachannel = (event) => {
    setupDataChannel(peerId, event.channel)
  }
  peerConnections.set(peerId, pc)
  return pc
}

async function initiateConnectionWith(peerId: string) {
  if (!webrtcEnabled.value || !ws.value || ws.value.readyState !== WebSocket.OPEN) {
    return
  }

  let pc = peerConnections.get(peerId)
  if (!pc) {
    pc = createPeerConnection(peerId)
  }

  if (pc.signalingState !== 'stable') {
    return
  }

  let channel = dataChannels.get(peerId)
  if (!channel || channel.readyState === 'closed') {
    channel = pc.createDataChannel('useebird-chat')
    setupDataChannel(peerId, channel)
  }

  const offer = await pc.createOffer()
  await pc.setLocalDescription(offer)
  sendSignalingMessage({
    type: 'webrtc-offer',
    target: peerId,
    sdp: JSON.stringify(offer),
  })
}

async function handleIncomingOffer(message: ServerMessage) {
  const peerId = message.sender
  if (!peerId || peerId === selfId.value || message.target !== selfId.value || !message.sdp) {
    return
  }

  let pc = peerConnections.get(peerId)
  if (!pc) {
    pc = createPeerConnection(peerId)
  }

  const desc = JSON.parse(message.sdp) as RTCSessionDescriptionInit
  await pc.setRemoteDescription(desc)
  const answer = await pc.createAnswer()
  await pc.setLocalDescription(answer)
  sendSignalingMessage({
    type: 'webrtc-answer',
    target: peerId,
    sdp: JSON.stringify(answer),
  })
}

async function handleIncomingAnswer(message: ServerMessage) {
  const peerId = message.sender
  if (!peerId || peerId === selfId.value || message.target !== selfId.value || !message.sdp) {
    return
  }

  const pc = peerConnections.get(peerId)
  if (!pc) {
    return
  }

  const desc = JSON.parse(message.sdp) as RTCSessionDescriptionInit
  await pc.setRemoteDescription(desc)
}

async function handleIncomingIceCandidate(message: ServerMessage) {
  const peerId = message.sender
  if (!peerId || peerId === selfId.value || message.target !== selfId.value || !message.candidate) {
    return
  }

  const pc = peerConnections.get(peerId)
  if (!pc) {
    return
  }

  try {
    const candidate = JSON.parse(message.candidate) as RTCIceCandidateInit
    await pc.addIceCandidate(candidate)
  } catch (error) {
    console.warn('Failed to add ICE candidate', error)
  }
}

function handleWebRTCPresenceRequest(message: ServerMessage) {
  const peerId = message.sender
  if (!peerId || peerId === selfId.value) {
    return
  }
  addKnownPeer(peerId)
  sendSignalingMessage({
    type: 'webrtc-presence',
    target: peerId,
    sentAt: new Date().toISOString(),
  })
  if (webrtcEnabled.value) {
    void initiateConnectionWith(peerId)
  }
}

function handleWebRTCPresence(message: ServerMessage) {
  const peerId = message.sender
  if (!peerId || peerId === selfId.value) {
    return
  }

  if (message.target && message.target !== selfId.value) {
    return
  }

  addKnownPeer(peerId)
}

function getOpenDataChannels() {
  return Array.from(dataChannels.entries()).filter(([, channel]) => channel.readyState === 'open')
}

function sendWebRTCPing() {
  if (!canSendWebRTCPing.value) {
    return
  }

  ensureWebRTCSetup()
  const openChannels = getOpenDataChannels()
  if (!openChannels.length) {
    appendMessage({
      id: crypto.randomUUID?.() ?? Math.random().toString(36).slice(2),
      type: 'system',
      text: 'WebRTC channel not ready yet — attempting to establish connection.',
      timestamp: new Date(),
    })
    return
  }

  openChannels.forEach(([peerId, channel]) => {
    const id = crypto.randomUUID?.() ?? Math.random().toString(36).slice(2)
    const entry: ChatLogEntry = {
      id,
      type: 'ping',
      text: `WebRTC ping sent to ${shortId(peerId)}…`,
      timestamp: new Date(),
      sender: selfId.value ?? undefined,
      pending: true,
    }
    appendMessage(entry)
    webrtcPendingPings.set(id, {
      startedAt: performance.now(),
      messageIndex: messages.value.length - 1,
      peerId,
    })
    channel.send(
      JSON.stringify({
        type: 'webrtc-ping',
        id,
        sentAt: new Date().toISOString(),
      }),
    )
  })
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

  if (parsed.sender) {
    addKnownPeer(parsed.sender)
  }

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
    case 'webrtc-presence-request':
      handleWebRTCPresenceRequest(parsed)
      break
    case 'webrtc-presence':
      handleWebRTCPresence(parsed)
      break
    case 'webrtc-offer':
      void handleIncomingOffer(parsed)
      break
    case 'webrtc-answer':
      void handleIncomingAnswer(parsed)
      break
    case 'webrtc-ice':
      void handleIncomingIceCandidate(parsed)
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

watch(
  () => messages.value.length,
  () => {
    void nextTick(() => {
      const container = logContainer.value
      if (container) {
        container.scrollTop = container.scrollHeight
      }
    })
  },
)

onMounted(() => {
  connect()
  void nextTick(() => {
    const container = logContainer.value
    if (container) {
      container.scrollTop = container.scrollHeight
    }
  })
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
      <div class="brand">
        <span class="brand-icon" aria-hidden="true"></span>
        <span class="sr-only">Realtime Chat</span>
      </div>
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
      <button
        type="button"
        class="webrtc"
        :disabled="!canSendWebRTCPing"
        @click="sendWebRTCPing"
      >
        WebRTC Ping
      </button>
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

.brand {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  position: relative;
}

.brand-icon {
  width: 2rem;
  height: 2rem;
  border-radius: 0.75rem;
  background: linear-gradient(135deg, #38bdf8, #6366f1);
  box-shadow: 0 10px 25px rgba(99, 102, 241, 0.35);
}

.sr-only {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
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
  min-height: 0;
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
  grid-template-columns: 1fr auto auto auto;
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

.input-bar button.webrtc {
  background: rgba(99, 102, 241, 0.2);
  color: #c7d2fe;
  box-shadow: 0 10px 25px rgba(99, 102, 241, 0.25);
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
