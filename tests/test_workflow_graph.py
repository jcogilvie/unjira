from unjira.jira.workflow import WorkflowGraph


def build_graph() -> WorkflowGraph:
    graph = WorkflowGraph()
    for _ in range(5):
        graph.observe("To Do", "In Progress")
        graph.observe("In Progress", "In Review")
        graph.observe("In Review", "Done")
    graph.observe("In Review", "In Progress")  # rare bounce-back
    return graph


def test_bfs_multi_hop_path():
    assert build_graph().path("To Do", "Done") == ["To Do", "In Progress", "In Review", "Done"]


def test_no_path_returns_none():
    assert build_graph().path("Done", "To Do") is None


def test_same_status_is_trivial_path():
    assert build_graph().path("Done", "Done") == ["Done"]


def test_rare_edges_flag_the_bounce_back():
    assert build_graph().rare_edges(max_count=1) == [("In Review", "In Progress", 1)]


def test_dict_roundtrip_preserves_counts():
    graph = build_graph()
    graph.add_status("To Do", "new")
    clone = WorkflowGraph.from_dict(graph.to_dict())
    assert clone.edges == graph.edges
    assert clone.status_categories == graph.status_categories
    assert clone.path("To Do", "Done") == graph.path("To Do", "Done")
