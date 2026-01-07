FROM python:3.12-slim-bookworm

ENV DEBIAN_FRONTEND=noninteractive
ENV PYTHONUNBUFFERED=1

RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc \
    g++ \
    gfortran \
    build-essential \
    libgl1 \
    libglib2.0-0 \
    libpango-1.0-0 \
    libpangocairo-1.0-0 \
    libgdk-pixbuf2.0-0 \
    libffi-dev \
    libcairo2 \
    ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && apt-get clean

RUN pip install --no-cache-dir --upgrade pip

RUN pip install --no-cache-dir \
    numpy \
    pandas \
    scipy \
    scikit-learn

RUN pip install --no-cache-dir \
    matplotlib \
    seaborn \
    plotly

RUN pip install --no-cache-dir \
    pillow \
    opencv-python-headless \
    moviepy \
    pydub

RUN pip install --no-cache-dir \
    weasyprint \
    reportlab \
    python-docx \
    openpyxl \
    python-pptx \
    PyPDF2

RUN pip install --no-cache-dir \
    beautifulsoup4 \
    requests

RUN useradd -m -u 1000 daytona

WORKDIR /home/daytona
RUN chown daytona:daytona /home/daytona

USER daytona

CMD ["/bin/bash"]
