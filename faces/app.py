"""faces finds the faces in a picture and tells what each looks like as a
vector, for title-api to tell who is on the screen (see its internal/faces).

It holds no state: one picture in, its faces out. The models are
InsightFace's buffalo_m, run by onnxruntime on the CPU: SCRFD-2.5G finds the
faces and their five points (eyes, nose, mouth corners), ArcFace R50
(w600k_r50) makes a face, aligned by those points, into 512 numbers whose
cosine with another face's tells whether it is the same person. On the
server's old Xeon SCRFD-10G took a second a picture, SCRFD-2.5G a seventh.

POST /v1/detect[?min=40&det=640&max=12], the body a JPEG or a PNG, answers
  {"model": "w600k_r50", "width": W, "height": H, "faces": [
    {"box": [x1, y1, x2, y2], "kps": [[x, y] × 5], "det": 0.87,
     "yaw": 0.05, "blur": 210.4, "embedding": "<512 float32 LE, base64>"}]}
the faces largest first; one smaller than min (pixels on its shorter side),
or past the max largest, has no embedding: ArcFace takes a second a face on
the server. det is the side the picture is made for the detector, 320
enough for a portrait. model names the vectors: those of another model
don't compare. GET /healthz answers ok once the models are loaded.
"""

import base64
import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

import cv2
import numpy as np
import onnxruntime as ort

MODELS = os.environ.get("FACES_MODELS", "/models")
MODEL = "w600k_r50"
THREADS = int(os.environ.get("FACES_THREADS", "2"))
ADDR = os.environ.get("FACES_ADDR", "127.0.0.1:50060")
# Pictures larger than this are refused.
MAX_BODY = 32 << 20

DET_SIZE = 640
DET_THRESH = 0.5
NMS_THRESH = 0.4
# Faces kept of a picture at most, the largest.
MAX_FACES = 12

# Where ArcFace wants the five points of a 112×112 face.
ARCFACE_DST = np.array(
    [[38.2946, 51.6963], [73.5318, 51.5014], [56.0252, 71.7366], [41.5493, 92.3655], [70.7299, 92.2041]],
    dtype=np.float32,
)


def session(name):
    options = ort.SessionOptions()
    options.intra_op_num_threads = THREADS
    options.inter_op_num_threads = 1
    # Threads spinning while they wait only eat the container's CPU quota.
    options.add_session_config_entry("session.intra_op.allow_spinning", "0")
    options.log_severity_level = 3
    return ort.InferenceSession(os.path.join(MODELS, name), options, providers=["CPUExecutionProvider"])


class Detector:
    """SCRFD with five points, strides 8, 16 and 32, two anchors each."""

    strides = (8, 16, 32)

    def __init__(self, name):
        self.session = session(name)
        self.input = self.session.get_inputs()[0].name
        self.outputs = [o.name for o in self.session.get_outputs()]
        self.centers = {}

    def anchors(self, height, width, stride):
        key = (height, width, stride)
        if key not in self.centers:
            grid = np.stack(np.mgrid[:height, :width][::-1], axis=-1).astype(np.float32)
            grid = (grid * stride).reshape((-1, 2))
            self.centers[key] = np.stack([grid] * 2, axis=1).reshape((-1, 2))
        return self.centers[key]

    def detect(self, image, size):
        h, w = image.shape[:2]
        if h > w:
            nh, nw = size, max(1, int(size * w / h))
        else:
            nw, nh = size, max(1, int(size * h / w))
        scale = nh / h
        canvas = np.zeros((size, size, 3), dtype=np.uint8)
        canvas[:nh, :nw] = cv2.resize(image, (nw, nh), interpolation=cv2.INTER_AREA)
        blob = cv2.dnn.blobFromImage(canvas, 1.0 / 128, (size, size), (127.5, 127.5, 127.5), swapRB=True)
        outs = self.session.run(self.outputs, {self.input: blob})
        if outs[0].ndim == 3:
            outs = [o[0] for o in outs]
        scores, boxes, points = [], [], []
        for i, stride in enumerate(self.strides):
            score = outs[i].reshape(-1)
            dist = outs[i + 3] * stride
            kps = outs[i + 6] * stride
            centers = self.anchors(size // stride, size // stride, stride)
            keep = np.where(score >= DET_THRESH)[0]
            if not len(keep):
                continue
            c, d, k = centers[keep], dist[keep], kps[keep]
            boxes.append(np.stack([c[:, 0] - d[:, 0], c[:, 1] - d[:, 1], c[:, 0] + d[:, 2], c[:, 1] + d[:, 3]], -1))
            points.append(np.stack([c[:, None, 0] + k[:, 0::2], c[:, None, 1] + k[:, 1::2]], -1))
            scores.append(score[keep])
        if not scores:
            return []
        scores = np.concatenate(scores)
        boxes = np.concatenate(boxes) / scale
        points = np.concatenate(points) / scale
        kept = nms(boxes, scores)
        return [(boxes[i], points[i], float(scores[i])) for i in kept]


def nms(boxes, scores):
    x1, y1, x2, y2 = boxes.T
    areas = (x2 - x1 + 1) * (y2 - y1 + 1)
    order = scores.argsort()[::-1]
    kept = []
    while order.size:
        i = order[0]
        kept.append(i)
        xx1 = np.maximum(x1[i], x1[order[1:]])
        yy1 = np.maximum(y1[i], y1[order[1:]])
        xx2 = np.minimum(x2[i], x2[order[1:]])
        yy2 = np.minimum(y2[i], y2[order[1:]])
        inter = np.maximum(0.0, xx2 - xx1 + 1) * np.maximum(0.0, yy2 - yy1 + 1)
        iou = inter / (areas[i] + areas[order[1:]] - inter)
        order = order[1:][iou <= NMS_THRESH]
    return kept


def similarity(src, dst):
    """The similarity transform (Umeyama) taking the points src onto dst."""
    mean_s, mean_d = src.mean(0), dst.mean(0)
    s, d = src - mean_s, dst - mean_d
    cov = d.T @ s / len(src)
    u, sigma, vt = np.linalg.svd(cov)
    sign = np.eye(2)
    if np.linalg.det(u) * np.linalg.det(vt) < 0:
        sign[1, 1] = -1
    rotation = u @ sign @ vt
    scale = (sigma * np.diag(sign)).sum() / s.var(0).sum()
    matrix = np.zeros((2, 3), dtype=np.float32)
    matrix[:, :2] = scale * rotation
    matrix[:, 2] = mean_d - scale * rotation @ mean_s
    return matrix


class Recognizer:
    def __init__(self, name):
        self.session = session(name)
        self.input = self.session.get_inputs()[0].name

    def embed(self, faces):
        blob = cv2.dnn.blobFromImages(faces, 1.0 / 127.5, (112, 112), (127.5, 127.5, 127.5), swapRB=True)
        out = self.session.run(None, {self.input: blob})[0]
        return out / np.linalg.norm(out, axis=1, keepdims=True)


def yaw(points):
    """How far the nose is from between the eyes, in eye distances: about 0
    facing the camera, past 0.5 turned well away."""
    left, right, nose = points[0], points[1], points[2]
    eyes = np.linalg.norm(right - left)
    return float((nose[0] - (left[0] + right[0]) / 2) / max(eyes, 1.0))


detector = None
recognizer = None
lock = threading.Lock()


def analyze(data, min_face, size, most):
    image = cv2.imdecode(np.frombuffer(data, dtype=np.uint8), cv2.IMREAD_COLOR)
    if image is None:
        raise ValueError("not a picture")
    with lock:
        found = detector.detect(image, size)
        found.sort(key=lambda f: -(f[0][2] - f[0][0]) * (f[0][3] - f[0][1]))
        found = found[:MAX_FACES]
        aligned, which = [], []
        faces = []
        for i, (box, points, score) in enumerate(found):
            side = min(box[2] - box[0], box[3] - box[1])
            face = {
                "box": [round(float(v), 1) for v in box],
                "kps": [[round(float(x), 1), round(float(y), 1)] for x, y in points],
                "det": round(score, 4),
                "yaw": round(yaw(points), 3),
            }
            if side >= min_face and len(aligned) < most:
                crop = cv2.warpAffine(image, similarity(points, ARCFACE_DST), (112, 112), borderValue=0.0)
                gray = cv2.cvtColor(crop, cv2.COLOR_BGR2GRAY)
                face["blur"] = round(float(cv2.Laplacian(gray, cv2.CV_64F).var()), 1)
                aligned.append(crop)
                which.append(i)
            faces.append(face)
        if aligned:
            for i, vector in zip(which, recognizer.embed(aligned)):
                faces[i]["embedding"] = base64.b64encode(vector.astype("<f4").tobytes()).decode()
    return {"model": MODEL, "width": image.shape[1], "height": image.shape[0], "faces": faces}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def reply(self, code, body):
        data = json.dumps(body, separators=(",", ":")).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if urlparse(self.path).path == "/healthz":
            self.reply(200, {"ok": recognizer is not None, "model": MODEL})
        else:
            self.reply(404, {"error": "not found"})

    def do_POST(self):
        url = urlparse(self.path)
        if url.path != "/v1/detect":
            self.reply(404, {"error": "not found"})
            return
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0 or length > MAX_BODY:
            self.reply(400, {"error": "a picture of up to 32 MB is wanted"})
            return
        data = self.rfile.read(length)
        query = parse_qs(url.query)
        try:
            min_face = float(query.get("min", ["40"])[0])
            size = int(query.get("det", [str(DET_SIZE)])[0])
            most = int(query.get("max", [str(MAX_FACES)])[0])
        except ValueError:
            self.reply(400, {"error": "bad min or det"})
            return
        if size not in (320, 480, 640, 800):
            self.reply(400, {"error": "det is one of 320, 480, 640, 800"})
            return
        started = time.monotonic()
        try:
            result = analyze(data, min_face, size, most)
        except ValueError as e:
            self.reply(400, {"error": str(e)})
            return
        result["ms"] = round((time.monotonic() - started) * 1000)
        self.reply(200, result)

    def log_message(self, format, *args):
        pass


def main():
    global detector, recognizer
    detector = Detector("det_2.5g.onnx")
    recognizer = Recognizer("w600k_r50.onnx")
    host, port = ADDR.rsplit(":", 1)
    server = ThreadingHTTPServer((host, int(port)), Handler)
    print(f"faces: {MODEL} on {THREADS} threads, listening on {ADDR}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
