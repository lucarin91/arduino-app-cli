FROM debian:trixie

RUN apt-get update \
    && apt-get install -y --no-install-recommends adbd file ssh sudo python3 \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m --create-home --shell /bin/bash --user-group --groups sudo arduino && \
    echo "arduino:arduino" | chpasswd && \
    mkdir /home/arduino/ArduinoApps && \
    chown -R arduino:arduino /home/arduino/ArduinoApps && \
    echo 'MaxSessions 64' >> /etc/ssh/sshd_config

COPY scripts/pong-server.py /usr/local/bin/pong-server.py
RUN chmod +x /usr/local/bin/pong-server.py

WORKDIR /home/arduino
EXPOSE 22

CMD ["/bin/sh", "-c", "/usr/sbin/sshd -D & su arduino -c adbd & /usr/local/bin/pong-server.py"]
