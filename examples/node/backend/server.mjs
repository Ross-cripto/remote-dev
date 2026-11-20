import { createServer } from "node:http";

const host = process.env.HOST ?? "0.0.0.0";
const port = Number(process.env.PORT ?? 4000);

const server = createServer((request, response) => {
  if (request.method !== "GET" || request.url !== "/api/hello") {
    response.writeHead(404);
    response.end();
    return;
  }
  response.writeHead(200, { "Content-Type": "application/json" });
  response.end(JSON.stringify({ message: "Hello from the Node backend on your Linux host." }));
});
server.listen(port, host);
