"""Independent complete Set/Delete/cache-write reconstruction for terminal proof."""
import base64
from collections import Counter
from verify_movement_rehearsal import require, commitment


class Journal:
    def __init__(self, exported=False):
        self.exported = exported
        self.canonical, self.frames, self.tips, self.block = {}, {}, {}, {}
        self.counts, self.heights = Counter(), 0

    def frame(self, store, view, parent=None):
        key = store, view
        if key not in self.frames:
            initial = self.frames[store, parent]['state'] if parent and (store, parent) in self.frames else self.canonical[store]
            self.frames[key] = dict(state=initial.copy(), dirty=set(), parent=parent)
        frame = self.frames[key]
        if parent is not None:
            require(frame['parent'] in (None, parent), 'native view parent changed')
            frame['parent'] = parent
        return frame

    def propagate(self, store, parent, key, value):
        for (name, view), child in list(self.frames.items()):
            if name == store and child['parent'] == parent and key not in child['dirty']:
                if value is None:
                    child['state'].pop(key, None)
                else:
                    child['state'][key] = value
                self.propagate(store, view, key, value)

    def accept(self, record):
        kind = record['kind']
        if kind == 'native_state_checkpoint':
            label = record['label'].split('/', 1)[1]
            state = {pair['key']: pair.get('value', '') for pair in record['before']}
            require(len(state) == len(record['before']), 'duplicate native anchor key')
            if record['label'].startswith(('segment_baseline/', 'export_baseline/')):
                require(label not in self.canonical, 'duplicate source anchor')
                self.canonical[label] = state
            elif record['label'].startswith(('segment_final/', 'export_final/')):
                actual = self.frame(label, self.tips[label])['state'] if self.exported and label in self.tips else self.canonical[label]
                require(actual == state, 'complete journal final state differs')
        if kind == 'native_write':
            store, key = record['label'].split('/key:', 1)
            require(store in self.canonical and base64.b64decode(key, validate=True), 'unknown native write store/key')
            view, parent = int(record['native_view']), int(record.get('native_parent', 0))
            require(view > 0 and parent != view, 'invalid native source view')
            frame = self.frame(store, view, parent)
            require(frame['state'].get(key) == record['before'], 'source write before-image differs')
            if record['write_operation'] == 'delete':
                require(record['after'] is None, 'native delete has value')
                frame['state'].pop(key, None)
            else:
                require(record['write_operation'] == 'set' and type(record['after']) is str, 'native set missing bytes')
                frame['state'][key] = record['after']
            frame['dirty'].add(key)
            self.propagate(store, view, key, record['after'])
            self.tips[store] = view
            self.counts[record['phase'] + '/' + record['status'] + '/' + record['write_operation']] += 1
        if kind == 'native_cache_write':
            store = record['label']
            view, parent = int(record['native_view']), int(record.get('native_parent', 0))
            require(record['phase'] in ('transaction', 'message', 'ante', 'begin_block', 'end_block', 'commit', 'zero_height_export'), 'cache flush phase missing')
            if record['phase'] == 'transaction':
                require(int(record['tx']) >= 0, 'nontransaction flush misclassified')
            child = self.frame(store, view, parent)
            if parent:
                dest = self.frame(store, parent)
                before, after = dest['state'], child['state'].copy()
                dest['state'] = after
                for key in set(before) | set(after):
                    if before.get(key) != after.get(key):
                        self.propagate(store, parent, key, after.get(key))
                child['dirty'].clear()
                child['state'] = after.copy()
                self.tips[store] = parent
            else:
                self.tips[store] = view
            if record['phase'] == 'commit':
                self.canonical[store] = self.frame(store, self.tips[store])['state'].copy()
        if kind == 'native_state_hash' and record['phase'] == 'block':
            self.block[record['label']] = record['after']
        if kind == 'height_completed':
            require(len(self.block) == len(self.canonical) == 9, 'complete journal height scope required')
            for store, state in self.canonical.items():
                raw_state = {base64.b64decode(key, validate=True): value for key, value in state.items()}
                actual = commitment(raw_state)
                require(actual['sha256'] == self.block[store]['sha256'] and actual['keys'] == int(self.block[store]['keys']), 'source writes/flush do not equal native committed state')
            self.frames.clear()
            self.tips.clear()
            self.block.clear()
            self.heights += 1
