import { ApiProxy, ConfigStore } from '@kinvolk/headlamp-plugin/lib';
import { historyPath, Hub, matchCluster, pickHub, proxyBase, Ref, Version } from './paths';

// The plugin reaches the mnemo hub through the Kubernetes API server's service proxy,
// with the user's own credentials: no CORS, no second login, and who may use it is
// Kubernetes RBAC (get on services/proxy for the hub's Service).

export const PLUGIN = 'mnemo-headlamp';

export type Conf = {
  cluster?: string; // the Headlamp cluster the hub runs in (default: the one being viewed)
  namespace?: string; // the hub's Service (default: found by label)
  service?: string;
  uiURL?: string; // mnemo's UI, for "Open in mnemo" links
};

export const store = new ConfigStore<Conf>(PLUGIN);

export type { Hub, Ref, Version } from './paths';
export { uiLink } from './paths';

function request(cluster: string | null, path: string) {
  // A 401 from the hub (its own login) must not log the user out of the cluster.
  return ApiProxy.request(path, { ...(cluster ? { cluster } : {}), autoLogoutOnAuthError: false });
}

// Where the hub is and what it calls each cluster rarely change: look them up once per
// session. A failed lookup is retried next time.
const cache = new Map<string, Promise<unknown>>();
function once<T>(key: string, f: () => Promise<T>): Promise<T> {
  let p = cache.get(key) as Promise<T> | undefined;
  if (!p) {
    p = f();
    p.catch(() => cache.delete(key));
    cache.set(key, p);
  }
  return p;
}

/** findHub returns where the hub is, or null if there is none to be found. */
export function findHub(conf: Conf, viewed: string | null): Promise<Hub | null> {
  const key = JSON.stringify(['hub', conf.cluster, conf.namespace, conf.service, viewed]);
  return once(key, async () => {
    const cluster = conf.cluster || null; // null: the viewed cluster
    if (conf.namespace && conf.service) {
      return { cluster, base: proxyBase(conf.namespace, conf.service) };
    }
    const list = await request(
      cluster,
      `/api/v1/services?${new URLSearchParams({ labelSelector: 'app.kubernetes.io/name=mnemo' })}`
    );
    const svc = pickHub(list?.items ?? []);
    return svc ? { cluster, base: proxyBase(svc.metadata.namespace, svc.metadata.name) } : null;
  });
}

/** mnemoCluster is the name mnemo records the viewed cluster under. */
export function mnemoCluster(hub: Hub, viewed: string | null): Promise<string | undefined> {
  return once(JSON.stringify(['cluster', hub, viewed]), async () => {
    const [clusters, ns] = await Promise.all([
      request(hub.cluster, hub.base + '/api/v1/clusters'),
      request(viewed, '/api/v1/namespaces/kube-system').catch(() => null),
    ]);
    return matchCluster(clusters ?? [], ns?.metadata?.uid, viewed);
  });
}

export function history(hub: Hub, ref: Ref): Promise<Version[]> {
  return request(hub.cluster, hub.base + historyPath(ref));
}
