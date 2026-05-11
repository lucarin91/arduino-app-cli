#!/usr/bin/env python3
"""Simple TCP server that replies "pong" to every connection."""

import socket
import sys

PORT = 9999


def main():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as srv:
        srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        srv.bind(("0.0.0.0", PORT))
        srv.listen()
        print(f"pong-server listening on :{PORT}", flush=True)
        while True:
            conn, addr = srv.accept()
            with conn:
                try:
                    conn.sendall(b"pong")
                    conn.shutdown(socket.SHUT_WR)  # send FIN so client's Read returns
                except OSError as e:
                    print(f"connection {addr} error: {e}", file=sys.stderr, flush=True)


if __name__ == "__main__":
    main()
