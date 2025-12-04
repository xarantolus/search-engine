from flask import Flask, request, jsonify
from sentence_transformers import SentenceTransformer
import os
from typing import List
from transformers import pipeline
from nltk.tokenize import sent_tokenize
import nltk
from numpy import mean
from waitress import serve
import logging
import time

logging.basicConfig(level=logging.INFO)

app = Flask(__name__)

# Load model from environment or default to a Hugging Face model
MODEL_NAME = os.getenv("MODEL", "sentence-transformers/multi-qa-MiniLM-L6-cos-v1")
MODEL_DIR = os.getenv("MODEL_DIR", "/tmp/sentence_transformers")
os.makedirs(MODEL_DIR, exist_ok=True)

logging.info(f"Loading model {MODEL_NAME}...")
model = SentenceTransformer(MODEL_NAME, trust_remote_code=True, cache_folder=MODEL_DIR)

# get context length
vector_len = model.get_sentence_embedding_dimension()
logging.info(f"Model has vector length {vector_len}")

@app.route("/v1/models", methods=["GET"])
@app.route("/models", methods=["GET"])
def list_models():
	return jsonify({
		"data": [
			{
				"id": MODEL_NAME,
				"object": "model",
				"model_type": "sentence-transformers",
				"object_type": "text_embedding"
			}
		]
	})

@app.route("/v1/embeddings", methods=["POST"])
@app.route("/embeddings", methods=["POST"])
def create_embeddings():
    data = request.json
    if "input" not in data:
        return jsonify({"error": "Missing 'input' field"}), 400

    if "model" in data and data["model"] != MODEL_NAME:
        return jsonify({"error": "Model not supported"}), 400

    input_data = data["input"]
    if isinstance(input_data, str):
        input_data = [input_data]

    # Extract prompt_name parameter for Snowflake-style prompts
    prompt_name = data.get("user")
    if prompt_name:
        logging.info(f"Using prompt_name: {prompt_name}")

    # For ~16KB texts, use small batch size
    batch_size = 100
    embeddings = []

    logging.info(f"Processing {len(input_data)} texts in batches of {batch_size}")
    start = time.time()

    for i in range(0, len(input_data), batch_size):
        batch = input_data[i:i+batch_size]
        logging.info(f"Processing batch {i//batch_size + 1}/{(len(input_data)-1)//batch_size + 1} with {len(batch)} texts")

        # Use prompt_name if provided
        if prompt_name:
            batch_embeddings = model.encode(batch, prompt_name=prompt_name).tolist()
        else:
            batch_embeddings = model.encode(batch).tolist()

        embeddings.extend(batch_embeddings)

    output_data = []
    for i, embedding in enumerate(embeddings):
        output_data.append({
            "object": "embedding",
            "embedding": embedding,
            "index": i
        })

    logging.info(f"Successfully processed {len(embeddings)} embeddings in {time.time() - start:.2f} seconds")
    return jsonify({
        "object": "list",
        "data": output_data,
        "model": MODEL_NAME,
    })

if __name__ == "__main__":
    port = int(os.getenv("PORT", 5000))
    if os.getenv("FLASK_ENV") == "development":
        logging.info(f"Starting debug server on port {port}...")
        app.run(host="0.0.0.0", port=port, debug=True)
    else:
        # Production mode: use Waitress WSGI server
        logging.info(f"Starting production server on port {port}...")
        serve(app, host="0.0.0.0", port=port, threads=4)
