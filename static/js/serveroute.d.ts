/**
 * Serveroute type declarations.
 */

/** Info returned about a single service. */
export interface ServiceInfo {
  active: boolean;
  stoppable: boolean;
}

/** SSE event callback. */
export type WatchEventCallback = (service: string, active: boolean) => void;

/** Handle returned by watch(), allowing the caller to unsubscribe. */
export interface WatchHandle {
  close(): void;
}

export default class Serveroute {
  /**
   * @param baseUrl  Base URL of the API endpoint, e.g.
   *     "//api.myserver.example.com".
   *                 Defaults to "//api.{window.location.host}".
   */
  constructor(baseUrl?: string);

  /** The current API base URL. */
  get baseUrl(): string;

  /** Override the API base URL at runtime. */
  setBaseUrl(url: string): void;

  /** GET / — list all non-hidden services. */
  list(): Promise<Record<string, ServiceInfo>>;

  /** GET /{name} — info for a single service. */
  get(name: string): Promise<ServiceInfo>;

  /** GET /{name}/active — check if a service is active. */
  isActive(name: string): Promise<boolean>;

  /** POST /{name}/active — start (true) or stop (false) a service. */
  setActive(name: string, active: boolean): Promise<boolean>;

  /** GET /:watch — subscribe to live state changes via SSE. */
  watch(onEvent: WatchEventCallback): WatchHandle;
}
