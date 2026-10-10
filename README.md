# LMDB

This repository contains three independent applications:

- [`castctl/`](castctl/) — minimal Chromecast controller with OpenWrt build files;
- [`movie-api/`](movie-api/) — Go gRPC service that resolves movie IDs to HLS streams;
- [`bff/`](bff/) — ucode CGI on the router that connects movie-api and castctl.

Build each application from its own directory.
