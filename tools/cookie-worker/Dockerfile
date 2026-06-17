FROM node:22-bookworm

WORKDIR /app

COPY package.json ./
RUN npm install --omit=dev && npx playwright install --with-deps chromium

COPY src ./src

ENV COOKIE_WORKER_HEADLESS=true
ENV COOKIE_WORKER_BROWSER_CHANNEL=

CMD ["npm", "start"]
