FROM python:3.14.7-slim AS runtime

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PORT=8080

WORKDIR /app

COPY services/litellm_adapter/requirements.txt ./requirements.txt
RUN pip install --no-cache-dir --requirement requirements.txt \
    && useradd --create-home --uid 10001 appuser

COPY services/litellm_adapter/app ./app
COPY pkg/contracts/testdata/chat ./fixtures

USER appuser
EXPOSE 8080
CMD ["sh", "-c", "uvicorn app.main:app --host 0.0.0.0 --port ${PORT}"]
