FROM python:3.13-slim
WORKDIR /app
COPY requirements.txt .
RUN --mount=type=cache,target=/root/.cache/pip \
	pip install -r requirements.txt
RUN --mount=type=cache,target=/root/.cache/pip \
	pip install "xformers>=0.0.29"

RUN python -c "import nltk; nltk.download('punkt')"
COPY . .
CMD ["python", "main.py"]
