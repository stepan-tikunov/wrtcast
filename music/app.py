"""music tells where music plays in a stretch of audio and what it is, for
title-api to show what plays in a film or an episode (see its internal/music).

It holds no state but the limiter of Shazam's requests: audio in, scores or a
track out. YAMNet (AudioSet's 521 classes, on 0.975 s windows of 16 kHz mono)
scores each half second for music, speech and song; it runs some forty times
faster than real time on the server. Shazam, through shazamio, is asked only
for the stretches title-api picks: its signature is made here, and only it
is sent.

POST /v1/classify, the body any audio ffmpeg reads (ADTS AAC as the router
cuts it out of a segment), answers
  {"model": "yamnet", "hop": 0.5, "window": 0.975, "duration": 4.01,
   "frames": [[t, music, speech, song], …], "features": [[c1, …, c12], …],
   "ms": 35}
t the start of each window in seconds from the start of the audio; song is
the highest of the classes of songs (singing, song, pop, rock, …). features
are each window's 12 MFCCs, its timbre without its loudness, for title-api
to tell where one song gives way to another (see its internal/music).

POST /v1/recognize, the body up to MAX_SECONDS of audio (8 to 12 s are
best), answers
  {"match": {"key", "title", "artist", "cover", "url", "isrc", "apple",
             "apple_url", "spotify", "offset", "timeskew", "frequencyskew",
             "duration"} | null, "ms": 900}
offset the seconds into the track where the audio starts, duration the
track's length from Apple (null when unknown). Shazam is asked at most
SHAZAM_MAX times in SHAZAM_WINDOW seconds; beyond that, after its 429 (for
SHAZAM_COOLDOWN seconds) or with MUSIC_SHAZAM=off the answer is 503
{"error": "limited" | "off", "retry_after": s}; nothing is retried here.
GET /healthz answers ok once YAMNet is loaded.
"""

import asyncio
import csv
import io
import json
import os
import re
import subprocess
import threading
import time
import wave
from collections import deque
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

import aiohttp
import numpy as np
from ai_edge_litert.interpreter import Interpreter
from aiohttp_retry import ExponentialRetry
from shazamio import HTTPClient, Shazam
from shazamio.exceptions import RateLimited

ADDR = os.environ.get("MUSIC_ADDR", "127.0.0.1:50061")
MODELS = os.environ.get("MUSIC_MODELS", "/models")
THREADS = int(os.environ.get("MUSIC_THREADS", "1"))
SHAZAM_ON = os.environ.get("MUSIC_SHAZAM", "on") != "off"
SHAZAM_MAX = int(os.environ.get("MUSIC_SHAZAM_MAX", "8"))
SHAZAM_WINDOW = float(os.environ.get("MUSIC_SHAZAM_WINDOW", "300"))
SHAZAM_COOLDOWN = float(os.environ.get("MUSIC_SHAZAM_COOLDOWN", "3600"))

RATE = 16000
HOP = 8000  # samples, half a second
MAX_BODY = 16 << 20
# Audio classified at most, and sent to Shazam at most (it wants 12 s).
MAX_CLASSIFY = 120
MAX_SECONDS = 20

SONG_CLASSES = {
    "Singing", "Song", "Vocal music", "Pop music", "Rock music", "Hip hop music", "Rhythm and blues",
    "Soul music", "Electronic music", "Dance music", "Disco", "Rapping", "Male singing", "Female singing",
    "Choir", "Funk", "Country", "Folk music", "Heavy metal", "Punk rock",
}


def decode(data, seconds):
    """The audio as float32 16 kHz mono, at most seconds long."""
    run = subprocess.run(
        ["ffmpeg", "-v", "error", "-i", "pipe:0", "-t", str(seconds), "-ac", "1", "-ar", str(RATE),
         "-f", "f32le", "pipe:1"],
        input=data, capture_output=True, timeout=60,
    )
    if run.returncode != 0:
        raise ValueError("unreadable audio: " + run.stderr.decode(errors="replace").strip()[-200:])
    pcm = np.frombuffer(run.stdout, np.float32)
    if len(pcm) == 0:
        raise ValueError("no audio")
    return pcm


class Yamnet:
    """YAMNet's TFLite classifier: one 0.975 s window in, 521 scores out."""

    def __init__(self):
        names = [row[2] for row in csv.reader(open(os.path.join(MODELS, "yamnet_class_map.csv")))][1:]
        self.music, self.speech = names.index("Music"), names.index("Speech")
        self.song = [i for i, n in enumerate(names) if n in SONG_CLASSES]
        self.interpreter = Interpreter(model_path=os.path.join(MODELS, "yamnet.tflite"), num_threads=THREADS)
        self.interpreter.allocate_tensors()
        self.input = self.interpreter.get_input_details()[0]
        self.output = self.interpreter.get_output_details()[0]
        self.size = int(self.input["shape"][-1])
        self.lock = threading.Lock()
        self.features = Features()

    def frames(self, pcm):
        if len(pcm) < self.size:
            pcm = np.concatenate([pcm, np.zeros(self.size - len(pcm), np.float32)])
        out, features = [], []
        with self.lock:
            for start in range(0, len(pcm) - self.size + 1, HOP):
                window = pcm[start:start + self.size].reshape(self.input["shape"])
                self.interpreter.set_tensor(self.input["index"], window)
                self.interpreter.invoke()
                p = self.interpreter.get_tensor(self.output["index"]).reshape(-1)
                out.append([round(start / RATE, 2), round(float(p[self.music]), 3),
                            round(float(p[self.speech]), 3), round(float(p[self.song].max()), 3)])
                features.append(self.features.of(pcm[start:start + self.size]))
        return out, features


class Features:
    """What a window sounds like, by numpy: its power spectrum averaged over
    64 ms frames as 12 MFCCs, of 40 mel bands from 60 Hz to 7.6 kHz, c0 (the
    loudness, no song's own) left out."""

    N_FFT, STEP, BANDS = 1024, 512, 40

    def __init__(self):
        freqs = np.fft.rfftfreq(self.N_FFT, 1 / RATE)
        mel = lambda f: 2595 * np.log10(1 + f / 700)
        edges = 700 * (10 ** (np.linspace(mel(60), mel(7600), self.BANDS + 2) / 2595) - 1)
        bank = np.zeros((self.BANDS, len(freqs)), np.float32)
        for b in range(self.BANDS):
            lo, mid, hi = edges[b], edges[b + 1], edges[b + 2]
            bank[b] = np.clip(np.minimum((freqs - lo) / (mid - lo), (hi - freqs) / (hi - mid)), 0, None)
        self.bank = bank
        n = np.arange(self.BANDS)
        self.dct = np.cos(np.pi / self.BANDS * (n + 0.5)[None, :] * np.arange(1, 13)[:, None]).astype(np.float32)
        self.hann = np.hanning(self.N_FFT).astype(np.float32)

    def of(self, window):
        count = 1 + (len(window) - self.N_FFT) // self.STEP
        index = np.arange(self.N_FFT)[None, :] + self.STEP * np.arange(count)[:, None]
        power = (np.abs(np.fft.rfft(window[index] * self.hann, axis=1)) ** 2).mean(axis=0)
        return [round(float(x), 2) for x in self.dct @ np.log(self.bank @ power + 1e-10)]


class Limited(Exception):
    def __init__(self, reason, retry_after):
        super().__init__(reason)
        self.reason, self.retry_after = reason, retry_after


class Limiter:
    """At most SHAZAM_MAX requests in any SHAZAM_WINDOW seconds, none for
    SHAZAM_COOLDOWN seconds after a 429: bursts pass, a steady stream
    doesn't."""

    def __init__(self):
        self.sent = deque()
        self.quiet_until = 0.0
        self.lock = threading.Lock()

    def take(self):
        if not SHAZAM_ON:
            raise Limited("off", None)
        with self.lock:
            now = time.time()
            if now < self.quiet_until:
                raise Limited("limited", round(self.quiet_until - now))
            while self.sent and self.sent[0] <= now - SHAZAM_WINDOW:
                self.sent.popleft()
            if len(self.sent) >= SHAZAM_MAX:
                raise Limited("limited", round(self.sent[0] + SHAZAM_WINDOW - now) + 1)
            self.sent.append(now)

    def throttled(self):
        with self.lock:
            self.quiet_until = time.time() + SHAZAM_COOLDOWN
        print(f"music: Shazam answered 429, quiet for {SHAZAM_COOLDOWN:.0f} s", flush=True)


def wav(pcm):
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(RATE)
        w.writeframes((np.clip(pcm, -1, 1) * 32767).astype("<i2").tobytes())
    return buf.getvalue()


def track_of(answer):
    """The first match of Shazam's answer, shortened; None without one."""
    track, matches = answer.get("track"), answer.get("matches") or []
    if not track or not matches:
        return None
    m = matches[0]
    hub = track.get("hub") or {}
    apple = apple_url = spotify = None
    for action in hub.get("actions") or []:
        if action.get("type") == "applemusicplay":
            apple = action.get("id")
    for option in hub.get("options") or []:
        for action in option.get("actions") or []:
            if action.get("type") == "applemusicopen":
                apple_url = re.sub(r"[?&](mttn|itscg|itsct)[^&]*", "", action.get("uri") or "") or None
    for provider in hub.get("providers") or []:
        if provider.get("type") == "SPOTIFY":
            spotify = ((provider.get("actions") or [{}])[0]).get("uri")
    return {
        "key": track.get("key"),
        "title": track.get("title"),
        "artist": track.get("subtitle"),
        "cover": (track.get("images") or {}).get("coverart"),
        "url": track.get("url"),
        "isrc": track.get("isrc"),
        "apple": apple,
        "apple_url": apple_url,
        "spotify": spotify,
        "offset": m.get("offset"),
        "timeskew": m.get("timeskew"),
        "frequencyskew": m.get("frequencyskew"),
        "duration": None,
    }


class Recognizer:
    """shazamio on an event loop of its own, retries off: a 429 is the
    limiter's to handle, not something to hammer through."""

    def __init__(self):
        self.loop = asyncio.new_event_loop()
        threading.Thread(target=self.loop.run_forever, daemon=True).start()
        self.limiter = Limiter()
        self.durations = {}
        self.shazam = self.run(self.make())

    async def make(self):
        client = HTTPClient(retry_options=ExponentialRetry(attempts=1, statuses=set()))
        self.session = aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=10))
        return Shazam(http_client=client)

    def run(self, coroutine):
        return asyncio.run_coroutine_threadsafe(coroutine, self.loop).result(timeout=60)

    async def duration(self, apple):
        """The length of an Apple Music track in seconds, from iTunes' open
        lookup; None when it can't tell."""
        if not apple:
            return None
        if apple not in self.durations:
            try:
                async with self.session.get("https://itunes.apple.com/lookup", params={"id": apple}) as r:
                    results = (json.loads(await r.text()) or {}).get("results") or [{}]
                    ms = results[0].get("trackTimeMillis")
                    self.durations[apple] = round(ms / 1000, 1) if ms else None
            except Exception as e:  # noqa: BLE001 — the length is a nicety
                print(f"music: iTunes lookup {apple}: {e!r}", flush=True)
                return None
        return self.durations[apple]

    async def ask(self, data):
        answer = await self.shazam.recognize(data)
        match = track_of(answer)
        if match:
            match["duration"] = await self.duration(match["apple"])
        return match

    def recognize(self, pcm):
        self.limiter.take()
        try:
            return self.run(self.ask(wav(pcm)))
        except RateLimited:
            self.limiter.throttled()
            raise Limited("limited", round(SHAZAM_COOLDOWN))


yamnet = None
recognizer = None


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def reply(self, code, body):
        data = json.dumps(body, separators=(",", ":"), ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if urlparse(self.path).path == "/healthz":
            self.reply(200, {"ok": yamnet is not None, "model": "yamnet", "shazam": SHAZAM_ON})
        else:
            self.reply(404, {"error": "not found"})

    def do_POST(self):
        path = urlparse(self.path).path
        if path not in ("/v1/classify", "/v1/recognize"):
            self.reply(404, {"error": "not found"})
            return
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0 or length > MAX_BODY:
            self.reply(400, {"error": "audio of up to 16 MB is wanted"})
            return
        data = self.rfile.read(length)
        started = time.monotonic()
        try:
            if path == "/v1/classify":
                pcm = decode(data, MAX_CLASSIFY)
                frames, features = yamnet.frames(pcm)
                result = {"model": "yamnet", "hop": HOP / RATE, "window": yamnet.size / RATE,
                          "duration": round(len(pcm) / RATE, 3), "frames": frames, "features": features}
            else:
                result = {"match": recognizer.recognize(decode(data, MAX_SECONDS))}
        except ValueError as e:
            self.reply(400, {"error": str(e)})
            return
        except Limited as e:
            self.reply(503, {"error": e.reason, "retry_after": e.retry_after})
            return
        except Exception as e:  # noqa: BLE001 — Shazam's or the network's failure
            print(f"music: {path}: {e!r}", flush=True)
            self.reply(502, {"error": repr(e)[:300]})
            return
        result["ms"] = round((time.monotonic() - started) * 1000)
        self.reply(200, result)

    def log_message(self, format, *args):
        pass


def main():
    global yamnet, recognizer
    yamnet = Yamnet()
    recognizer = Recognizer()
    host, port = ADDR.rsplit(":", 1)
    server = ThreadingHTTPServer((host, int(port)), Handler)
    print(f"music: yamnet, Shazam {'on' if SHAZAM_ON else 'off'} ({SHAZAM_MAX} in {SHAZAM_WINDOW:.0f} s), "
          f"listening on {ADDR}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
