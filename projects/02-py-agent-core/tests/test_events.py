from py_agent_core.events import TextEvent, parse_event


def test_valid():
    ev, err = parse_event('{"type":"text","delta":"你好"}')
    assert err is None
    assert isinstance(ev, TextEvent)
    assert ev.delta == "你好"


def test_invalid():
    for bad in ['{"type":"text"}', '{"type":"unknown"}', "not json"]:
        ev, err = parse_event(bad)
        assert ev is None
        assert err
