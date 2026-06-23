/**
 * Serveroute — modern ES client for the serveroute REST API.
 *
 * Constructor accepts an optional baseUrl. If omitted, the default is
 *   //api.${window.location.host}
 *
 * Example:
 *   import Serveroute from './serveroute.js';
 *   const api = new Serveroute();
 *   const services = await api.list();
 */

class Serveroute {
  #baseUrl;

  /**
   * @param {string} [baseUrl]  Base URL of the API endpoint, e.g.
   *     "//api.myserver.example.com"
   *                            Trailing slashes are stripped automatically.
   */
  constructor(baseUrl) {
    if (baseUrl) {
      this.#baseUrl = baseUrl.replace(/\/+$/, '');
    } else {
      this.#baseUrl = `//api.${window.location.host}`;
    }
  }

  /** @returns {string} The current base URL (read-only). */
  get baseUrl() { return this.#baseUrl; }

  /**
   * Change the API base URL at runtime.
   * @param {string} url
   */
  setBaseUrl(url) { this.#baseUrl = url.replace(/\/+$/, ''); }

  // -------------------------------------------------------------------
  // Internal helpers
  // -------------------------------------------------------------------

  /**
   * @param {string} path
   * @param {RequestInit} [options]
   * @returns {Promise<Response>}
   */
  async #request(path, options = {}) {
    const url = `${this.#baseUrl}${path}`;
    const res = await fetch(url, options);
    if (!res.ok) {
      let detail = res.statusText;
      try {
        const body = await res.text();
        if (body)
          detail = body;
      } catch {
        // ignore body read errors
      }
      throw new Error(`API ${res.status}: ${detail}`);
    }
    return res;
  }

  // -------------------------------------------------------------------
  // REST endpoints
  // -------------------------------------------------------------------

  /**
   * GET / — list every non-hidden service on the API host.
   *
   * @returns {Promise<Record<string, {active: boolean, stoppable: boolean}>>}
   */
  async list() {
    const res = await this.#request('/');
    return res.json();
  }

  /**
   * GET /{name} — info for a single service.
   *
   * @param {string} name  service (subdomain) name
   * @returns {Promise<{active: boolean, stoppable: boolean}>}
   */
  async get(name) {
    const res = await this.#request(`/${encodeURIComponent(name)}`);
    return res.json();
  }

  /**
   * GET /{name}/active — check if a systemd-backed service is active.
   *
   * @param {string} name  service name
   * @returns {Promise<boolean>}
   */
  async isActive(name) {
    const res = await this.#request(`/${encodeURIComponent(name)}/active`);
    const {active} = await res.json();
    return active;
  }

  /**
   * POST /{name}/active — start or stop a systemd-backed service.
   *
   * @param {string} name    service name
   * @param {boolean} active  true to start, false to stop
   * @returns {Promise<boolean>}  resulting active state
   */
  async setActive(name, active) {
    const res = await this.#request(`/${encodeURIComponent(name)}/active`, {
      method : 'POST',
      headers : {'Content-Type' : 'application/json'},
      body : JSON.stringify({active}),
    });
    const data = await res.json();
    return data.active;
  }

  // -------------------------------------------------------------------
  // SSE stream
  // -------------------------------------------------------------------

  /**
   * GET /:watch — subscribe to live systemd state changes via SSE.
   *
   * The callback receives (service, active) for every state change.
   * Returns a handle with a `close()` method to unsubscribe.
   *
   * @param {(service: string, active: boolean) => void} onEvent
   * @returns {{ close: () => void }}
   */
  watch(onEvent) {
    const url = `${this.#baseUrl}/:watch`;
    const es = new EventSource(url);

    es.onmessage = (e) => {
      try {
        const {service, active} = JSON.parse(e.data);
        onEvent(service, active);
      } catch {
        // ignore malformed events
      }
    };

    return {
      close() { es.close(); },
    };
  }
}

export default Serveroute;
