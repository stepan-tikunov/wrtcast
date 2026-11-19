# InsightFace's buffalo_m models (non-commercial use only) are downloaded at
# build time, not kept in the repository; only the two used are kept.
FROM python:3.12-slim AS models
RUN python -c "import urllib.request; urllib.request.urlretrieve('https://github.com/deepinsight/insightface/releases/download/v0.7/buffalo_m.zip', '/buffalo_m.zip')" \
	&& mkdir /models \
	&& python -c "import zipfile; z = zipfile.ZipFile('/buffalo_m.zip'); [open('/models/' + n.split('/')[-1], 'wb').write(z.read(n)) for n in z.namelist() if n.split('/')[-1] in ('det_2.5g.onnx', 'w600k_r50.onnx')]" \
	&& ls -la /models

FROM python:3.12-slim
COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt
COPY --from=models /models /models
COPY app.py /app/app.py
USER 65532:65532
ENTRYPOINT ["python", "-u", "/app/app.py"]
