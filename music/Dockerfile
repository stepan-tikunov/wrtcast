# YAMNet (Apache 2.0) is downloaded at build time, not kept in the
# repository: the TFLite classifier and AudioSet's class names.
FROM python:3.12-slim AS models
RUN mkdir /models && python -c "import urllib.request as u; \
u.urlretrieve('https://storage.googleapis.com/download.tensorflow.org/models/tflite/task_library/audio_classification/android/lite-model_yamnet_classification_tflite_1.tflite', '/models/yamnet.tflite'); \
u.urlretrieve('https://raw.githubusercontent.com/tensorflow/models/master/research/audioset/yamnet/yamnet_class_map.csv', '/models/yamnet_class_map.csv')" \
	&& ls -la /models

FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg && rm -rf /var/lib/apt/lists/*
COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt
COPY --from=models /models /models
COPY app.py /app/app.py
USER 65532:65532
ENTRYPOINT ["python", "-u", "/app/app.py"]
