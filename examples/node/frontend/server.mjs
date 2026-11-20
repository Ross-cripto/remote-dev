import { readFile } from "node:fs/promises";
import { createServer } from "node:http";

const host = process.env.HOST ?? "0.0.0.0";
const port = Number(process.env.PORT ?? 3000);
const backendUrl = process.env.BACKEND_URL ?? "http://localhost:4000";
const page = new URL("./index.html", import.meta.url);
const backendTimeout = 5_000;

async function handleRequest(request, response) {
  if (request.method === "GET" && request.url === "/") {
    const body = await readFile(page);
    response.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
    response.end(body);
    return;
  }
  if (request.method === "GET" && request.url === "/api/hello") {
    const backend = await fetch(new URL(request.url, backendUrl), {
      signal: AbortSignal.timeout(backendTimeout),
    });
    const body = await backend.text();
    response.writeHead(backend.status, { "Content-Type": "application/json" });
    response.end(body);
    return;
  }
  response.writeHead(404);
  response.end();
}

const server = createServer((request, response) => {
  handleRequest(request, response).catch((error) => {
    console.error(error);
    response.writeHead(502, { "Content-Type": "application/json" });
    response.end(JSON.stringify({ error: "The frontend could not load the requested resource." }));
  });
});
server.listen(port, host);
